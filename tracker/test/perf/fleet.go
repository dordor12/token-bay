//go:build perf

package perf

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/token-bay/token-bay/tracker/test/e2e/driver"
	"github.com/token-bay/token-bay/tracker/test/perf/report"
)

// trackerImage is the same image the e2e suite builds.
const trackerImage = "token-bay-tracker:dev"

// perfAdminToken guards the fleet's admin APIs. Test-topology secret,
// same posture as the e2e compose file's tokens.
const perfAdminToken = "perf-admin-token"

// liveTracker is one running fleet member plus its host-mapped
// endpoints.
type liveTracker struct {
	Node      *trackerNode
	Container testcontainers.Container
	RPCAddr   string // host:port (udp) for driver.DialRPCVia
	Admin     *driver.Admin
	MetricsAt string // http://host:port
}

// Fleet manages the growing tracker topology: all nodes pre-rendered,
// containers started staggered (spec §4).
type Fleet struct {
	nodes  []*trackerNode
	genDir string
	netwrk *testcontainers.DockerNetwork

	mu   sync.RWMutex
	live []*liveTracker
	died []string
}

// NewFleet renders every node config and creates the docker network.
func NewFleet(ctx context.Context, genDir string, trackersMax int) (*Fleet, error) {
	nodes, err := renderFleet(genDir, trackersMax)
	if err != nil {
		return nil, err
	}
	nw, err := tcnetwork.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("perf: create docker network: %w", err)
	}
	return &Fleet{nodes: nodes, genDir: genDir, netwrk: nw}, nil
}

// StartNext launches the next not-yet-started tracker container and
// waits for its health endpoint. Returns false when the fleet is at
// its cap.
func (f *Fleet) StartNext(ctx context.Context) (bool, error) {
	f.mu.RLock()
	next := len(f.live)
	f.mu.RUnlock()
	if next >= len(f.nodes) {
		return false, nil
	}
	node := f.nodes[next]

	req := testcontainers.ContainerRequest{
		Image: trackerImage,
		Cmd:   []string{"run", "--config", fmt.Sprintf("/gen/tracker-%d.yaml", node.Index)},
		Env:   map[string]string{"TOKEN_BAY_ADMIN_TOKEN": perfAdminToken},
		ExposedPorts: []string{
			"7777/udp", // QUIC RPC
			"9090/tcp", // admin
			"9100/tcp", // metrics
		},
		Networks:       []string{f.netwrk.Name},
		NetworkAliases: map[string][]string{f.netwrk.Name: {node.Name}},
		HostConfigModifier: func(hc *container.HostConfig) {
			hc.Binds = append(hc.Binds, f.genDir+":/gen:ro")
		},
		WaitingFor: wait.ForHTTP("/health").
			WithPort("9090/tcp").
			WithHeaders(map[string]string{"Authorization": "Bearer " + perfAdminToken}).
			WithStartupTimeout(120 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return false, fmt.Errorf("perf: start %s: %w", node.Name, err)
	}

	host, err := c.Host(ctx)
	if err != nil {
		return false, fmt.Errorf("perf: %s host: %w", node.Name, err)
	}
	rpcPort, err := c.MappedPort(ctx, "7777/udp")
	if err != nil {
		return false, fmt.Errorf("perf: %s rpc port: %w", node.Name, err)
	}
	adminPort, err := c.MappedPort(ctx, "9090/tcp")
	if err != nil {
		return false, fmt.Errorf("perf: %s admin port: %w", node.Name, err)
	}
	metricsPort, err := c.MappedPort(ctx, "9100/tcp")
	if err != nil {
		return false, fmt.Errorf("perf: %s metrics port: %w", node.Name, err)
	}

	lt := &liveTracker{
		Node:      node,
		Container: c,
		RPCAddr:   fmt.Sprintf("%s:%s", host, rpcPort.Port()),
		Admin:     driver.NewAdmin(fmt.Sprintf("http://%s:%s", host, adminPort.Port()), perfAdminToken),
		MetricsAt: fmt.Sprintf("http://%s:%s", host, metricsPort.Port()),
	}
	f.mu.Lock()
	f.live = append(f.live, lt)
	f.mu.Unlock()
	return true, nil
}

// Live snapshots the running trackers.
func (f *Fleet) Live() []*liveTracker {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]*liveTracker, len(f.live))
	copy(out, f.live)
	return out
}

// CheckHealth polls every live tracker; a tracker whose container is
// no longer running is recorded as died (permanently), an unhealthy
// but running one only counts against TrackersHealthy.
func (f *Fleet) CheckHealth(ctx context.Context) (healthy int) {
	for _, lt := range f.Live() {
		state, err := lt.Container.State(ctx)
		if ctx.Err() != nil {
			// The run window closed mid-check: a context error is a
			// shutdown race, never a death verdict.
			return healthy
		}
		if err != nil || !state.Running {
			f.recordDeath(lt.Node.Name)
			continue
		}
		hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		h, err := lt.Admin.Health(hctx)
		cancel()
		if err == nil && h.Status == "ok" {
			healthy++
		}
	}
	return healthy
}

func (f *Fleet) recordDeath(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.died {
		if d == name {
			return
		}
	}
	f.died = append(f.died, name)
}

// Died lists trackers whose containers stopped running during the run.
func (f *Fleet) Died() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]string, len(f.died))
	copy(out, f.died)
	return out
}

// ScrapeFinals collects each live tracker's end-of-run metrics.
func (f *Fleet) ScrapeFinals(ctx context.Context) []report.TrackerFinal {
	var out []report.TrackerFinal
	for _, lt := range f.Live() {
		tf := report.TrackerFinal{Name: lt.Node.Name}
		if m, err := scrapeMetrics(ctx, lt.MetricsAt); err == nil {
			tf.BrokerDecisions = report.SumMetric(m, "broker_submit_decisions_total")
			tf.InflightCount = report.SumMetric(m, "broker_inflight_count")
			tf.AdmissionQueueDepth = report.SumMetric(m, "admission_queue_depth")
			tf.FedPeersSteady = report.MetricLabeled(m, "tokenbay_federation_peers", `state="steady"`)
			if cnt := report.SumMetric(m, "broker_submit_duration_seconds_count"); cnt > 0 {
				tf.BrokerSubmitAvgMs = report.SumMetric(m, "broker_submit_duration_seconds_sum") / cnt * 1000
			}
		}
		out = append(out, tf)
	}
	return out
}

// FedSteadyEdges counts directed steady peering edges between live
// fleet members (admin /peers), and the expected full-mesh edge count.
func (f *Fleet) FedSteadyEdges(ctx context.Context) (steady, expected int) {
	live := f.Live()
	fedIDs := make(map[string]bool, len(live))
	for _, lt := range live {
		fedIDs[lt.Node.FedIDHex()] = true
	}
	expected = len(live) * (len(live) - 1)
	for _, lt := range live {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		peers, err := lt.Admin.Peers(pctx)
		cancel()
		if err != nil {
			continue
		}
		for _, p := range peers.Peers {
			if p.State == "steady" && fedIDs[p.TrackerID] {
				steady++
			}
		}
	}
	return steady, expected
}

// DumpLogs writes each live container's logs through sink (one call
// per tracker) for CI artifact collection.
func (f *Fleet) DumpLogs(ctx context.Context, sink func(name string, logs io.Reader)) {
	for _, lt := range f.Live() {
		rc, err := lt.Container.Logs(ctx)
		if err != nil {
			continue
		}
		sink(lt.Node.Name, rc)
		_ = rc.Close()
	}
}

// Down terminates every container and removes the network.
func (f *Fleet) Down(ctx context.Context) {
	for _, lt := range f.Live() {
		_ = lt.Container.Terminate(ctx)
	}
	if f.netwrk != nil {
		_ = f.netwrk.Remove(ctx)
	}
}

// scrapeMetrics fetches and parses one /metrics endpoint.
func scrapeMetrics(ctx context.Context, baseURL string) (map[string]float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/metrics", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("perf: metrics scrape %s: HTTP %d", baseURL, resp.StatusCode)
	}
	return report.ParsePromText(resp.Body)
}

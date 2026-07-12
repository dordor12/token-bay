//go:build perf

// Package perf is the tracker performance/soak harness (spec
// docs/superpowers/specs/tracker/2026-07-12-tracker-perf-ci-design.md).
// It drives thousands of simulated consumers/seeders against a growing
// fleet of real tracker containers for a configurable duration
// (PERF_DURATION, default 1h) and fails on hard health thresholds.
//
// Run via `make -C tracker test-perf`; parameters are env vars (spec
// §7). Everything here needs Docker and the token-bay-tracker:dev
// image, hence the perf build tag.
package perf

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/token-bay/token-bay/tracker/internal/broker"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/test/perf/loadgen"
	"github.com/token-bay/token-bay/tracker/test/perf/report"
)

// drainGrace is how long clients stay alive after the measured window
// so in-flight settlements complete before the final snapshot.
const drainGrace = 30 * time.Second

// heartbeatEvery keeps 20k QUIC connections inside the server's 60s
// idle timeout and seeder records fresh for admission supply.
const heartbeatEvery = 20 * time.Second

// closer is anything the teardown must Close.
type closer interface{ Close() }

func TestTrackerPerformanceSoak(t *testing.T) {
	params, err := report.ParamsFromEnv(os.Getenv)
	if err != nil {
		t.Fatalf("perf: %v", err)
	}
	t.Logf("perf: duration=%s consumers=%d seeders=%d trackers=%d..%d add-every=%s request-every=%s",
		params.Duration, params.Consumers, params.Seeders,
		params.TrackersInitial, params.TrackersMax, params.TrackerAddInterval, params.RequestInterval)

	if err := os.MkdirAll(params.ReportDir, 0o755); err != nil {
		t.Fatalf("perf: mkdir report dir: %v", err)
	}

	ctx := context.Background()
	fleet, err := NewFleet(ctx, t.TempDir(), params.TrackersMax)
	if err != nil {
		t.Fatalf("perf: %v", err)
	}
	defer fleet.Down(context.Background())
	defer dumpFleetLogs(fleet, params.ReportDir, t)

	for i := 0; i < params.TrackersInitial; i++ {
		if _, err := fleet.StartNext(ctx); err != nil {
			t.Fatalf("perf: start initial tracker %d: %v", i, err)
		}
	}
	t.Logf("perf: %d initial tracker(s) healthy", params.TrackersInitial)

	pool, err := loadgen.NewTransportPool(params.Sockets)
	if err != nil {
		t.Fatalf("perf: %v", err)
	}
	defer pool.Close()

	// The same pricing the rendered configs carry (both come from
	// config.DefaultConfig), so client-side cost math can never drift
	// from what the trackers recompute at settlement.
	prices := broker.NewPriceTableFromConfig(config.DefaultConfig().Pricing)
	counters := &loadgen.Counters{}

	// runCtx is the measured window; lifeCtx outlives it by drainGrace.
	lifeCtx, endLife := context.WithCancel(ctx)
	defer endLife()
	runCtx, endRun := context.WithTimeout(lifeCtx, params.Duration)
	defer endRun()

	var (
		handleMu sync.Mutex
		handles  []closer
	)
	track := func(c closer) {
		handleMu.Lock()
		handles = append(handles, c)
		handleMu.Unlock()
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); spawnLoop(runCtx, lifeCtx, t, params, fleet, pool, prices, counters, track) }()
	go func() { defer wg.Done(); growLoop(runCtx, t, params, fleet) }()
	go func() { defer wg.Done(); healthLoop(runCtx, params, fleet) }()

	<-runCtx.Done()
	t.Logf("perf: measured window over; draining %s before the final snapshot", drainGrace)
	wg.Wait()
	time.Sleep(drainGrace)

	// --- Final snapshot -------------------------------------------------
	var s report.Summary
	s.Duration = params.Duration
	s.Consumers = params.Consumers
	s.Seeders = params.Seeders
	counters.Snapshot(&s)
	s.TrackersFinal = len(fleet.Live())
	s.TrackersHealthy = fleet.CheckHealth(ctx)
	s.TrackersDied = fleet.Died()
	s.TrackerFinals = fleet.ScrapeFinals(ctx)
	s.FedSteadyEdges, s.FedExpectedEdges = fleet.FedSteadyEdges(ctx)

	endLife()
	for _, h := range handles {
		h.Close()
	}

	violations := report.Evaluate(s, params.Thresholds)
	md := report.RenderMarkdown(s, violations)
	t.Logf("perf report:\n%s", md)
	if err := os.WriteFile(filepath.Join(params.ReportDir, "report.md"), []byte(md), 0o644); err != nil { //nolint:gosec // CI artifact
		t.Errorf("perf: write report.md: %v", err)
	}
	if err := report.WriteJSON(filepath.Join(params.ReportDir, "summary.json"), s, violations); err != nil {
		t.Errorf("perf: %v", err)
	}
	for _, v := range violations {
		t.Errorf("perf threshold violation [%s]: %s", v.Name, v.Detail)
	}
}

// spawnLoop ramps the population up over the first 80%% of the run:
// every tick it computes how many seeders/consumers should exist by
// now and spawns the deficit, round-robin over the trackers live at
// spawn time — which is how newly-joined trackers organically receive
// load (spec §3).
func spawnLoop(runCtx, lifeCtx context.Context, t *testing.T, params report.Params,
	fleet *Fleet, pool *loadgen.TransportPool, prices *broker.PriceTable,
	counters *loadgen.Counters, track func(closer),
) {
	rampWindow := params.Duration * 8 / 10
	if rampWindow <= 0 {
		rampWindow = params.Duration
	}
	start := time.Now()
	var spawnedSeeders, spawnedConsumers int
	var rr int // round-robin cursor over live trackers

	// sem bounds concurrent dial+enroll handshakes.
	sem := make(chan struct{}, 16)
	var spawnWG sync.WaitGroup
	defer spawnWG.Wait()

	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-runCtx.Done():
			return
		case <-tick.C:
		}
		frac := float64(time.Since(start)) / float64(rampWindow)
		if frac > 1 {
			frac = 1
		}
		wantSeeders := int(frac * float64(params.Seeders))
		wantConsumers := int(frac * float64(params.Consumers))

		live := fleet.Live()
		if len(live) == 0 {
			continue
		}
		// Cap the per-tick burst so a stalled tick never stampedes.
		const maxBurst = 200
		burst := 0
		for spawnedSeeders < wantSeeders && burst < maxBurst {
			lt := live[rr%len(live)]
			rr++
			spawnedSeeders++
			burst++
			spawnWG.Add(1)
			go func() {
				defer spawnWG.Done()
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-runCtx.Done():
					return
				}
				s, err := loadgen.StartSeeder(lifeCtx, loadgen.SeederOpts{
					TrackerAddr:    lt.RPCAddr,
					SPKI:           lt.Node.SPKIHash,
					Transport:      pool.Next(),
					Model:          params.Model,
					Headroom:       1.0,
					HeartbeatEvery: heartbeatEvery,
					AdvertiseEvery: time.Minute,
					ServeDelay:     100 * time.Millisecond,
					Prices:         prices,
					Counters:       counters,
				})
				if err == nil {
					track(s)
				}
			}()
		}
		for spawnedConsumers < wantConsumers && burst < maxBurst {
			lt := live[rr%len(live)]
			rr++
			spawnedConsumers++
			burst++
			spawnWG.Add(1)
			go func() {
				defer spawnWG.Done()
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-runCtx.Done():
					return
				}
				c, err := loadgen.StartConsumer(lifeCtx, runCtx, loadgen.ConsumerOpts{
					TrackerAddr:    lt.RPCAddr,
					SPKI:           lt.Node.SPKIHash,
					Transport:      pool.Next(),
					Model:          params.Model,
					MaxIn:          1,
					MaxOut:         1,
					RequestEvery:   params.RequestInterval,
					HeartbeatEvery: heartbeatEvery,
					Prices:         prices,
					Counters:       counters,
				})
				if err == nil {
					track(c)
				}
			}()
		}
		if spawnedSeeders == params.Seeders && spawnedConsumers == params.Consumers {
			t.Logf("perf: full population spawned (%d seeders, %d consumers) after %s",
				spawnedSeeders, spawnedConsumers, time.Since(start).Round(time.Second))
			return
		}
	}
}

// growLoop adds one tracker to the federation every TrackerAddInterval
// until the cap — the "new tracker keeps joining the network" half of
// the scenario.
func growLoop(runCtx context.Context, t *testing.T, params report.Params, fleet *Fleet) {
	start := time.Now()
	tick := time.NewTicker(params.TrackerAddInterval)
	defer tick.Stop()
	for {
		select {
		case <-runCtx.Done():
			return
		case <-tick.C:
		}
		started, err := fleet.StartNext(runCtx)
		if err != nil {
			if runCtx.Err() == nil {
				t.Errorf("perf: tracker join failed: %v", err)
			}
			return
		}
		if !started {
			return // fleet at cap
		}
		t.Logf("perf: tracker %d joined the network (%s elapsed)",
			len(fleet.Live())-1, time.Since(start).Round(time.Second))
	}
}

// healthLoop records container deaths as they happen so a mid-run
// crash is attributed even if the container restarts policy-free.
func healthLoop(runCtx context.Context, params report.Params, fleet *Fleet) {
	interval := params.ScrapeInterval * 2
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-runCtx.Done():
			return
		case <-tick.C:
		}
		fleet.CheckHealth(runCtx)
	}
}

// dumpFleetLogs writes each tracker's container log into the report
// dir for artifact upload.
func dumpFleetLogs(fleet *Fleet, reportDir string, t *testing.T) {
	logDir := filepath.Join(reportDir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Logf("perf: mkdir %s: %v", logDir, err)
		return
	}
	fleet.DumpLogs(context.Background(), func(name string, logs io.Reader) {
		f, err := os.Create(filepath.Join(logDir, fmt.Sprintf("%s.log", name))) //nolint:gosec // fixed name pattern
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = io.Copy(f, logs)
	})
}

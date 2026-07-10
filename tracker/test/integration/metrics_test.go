//go:build integration

package integration_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/tracker/internal/federation"
)

// TestMetricsEndpoint_ServesRegisteredCollectorsUnauthenticated exercises
// the P6 /metrics wiring: a bare http.ServeMux with only "/metrics"
// mounted to promhttp.HandlerFor(...), backed by an http.Server built,
// started, and shut down the same way cmd/token-bay-tracker/run_cmd.go's
// buildMetricsServer does for the real tracker process.
//
// Booting the full `run` composition (identity key files, TLS certs,
// SQLite ledger/admission paths, federation transport, ...) just to hit
// one HTTP endpoint would add a large fixture for no additional coverage
// of the thing this test cares about: does the metrics listener render a
// genuinely-registered collector, and does it require no auth. So this
// test stands up that listener in isolation, seeded with a real
// production collector (federation.Metrics, the same constructor
// run_cmd.go calls for the live federation subsystem) registered against
// a real prometheus.Registry, and drives it with a plain HTTP client that
// never sets an Authorization header.
func TestMetricsEndpoint_ServesRegisteredCollectorsUnauthenticated(t *testing.T) {
	reg := prometheus.NewRegistry()

	// Register a real subsystem collector, exactly as run_cmd.go does at
	// startup ("Metrics: federation.NewMetrics(prometheus.DefaultRegisterer)"
	// in the federation.Open call). tokenbay_federation_dedupe_size is a
	// plain Gauge (not a *Vec), so it appears in the exposition body at its
	// zero value immediately on registration — no need to touch a label
	// first to make it observable.
	federation.NewMetrics(reg)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	})

	req, err := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+"/metrics", nil)
	require.NoError(t, err)
	// Deliberately no Authorization header: Prometheus scrapers (and the
	// e2e assertion surface) never send the admin bearer token, so
	// /metrics must serve 200 without one.

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(body), "tokenbay_federation_dedupe_size")
}

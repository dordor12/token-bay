package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/token-bay/token-bay/tracker/internal/config"
)

// TestBuildMetricsServer_ServesMetricsUnauthenticated exercises the exact
// production helper run_cmd.go uses to build the third (metrics) HTTP
// listener: a bare mux with only "/metrics" wired to promhttp against
// prometheus.DefaultGatherer, no bearer-token guard.
//
// client_golang registers the process and Go collectors onto
// prometheus.DefaultRegisterer unconditionally at package init (see
// github.com/prometheus/client_golang/prometheus/registry.go's init()),
// so "go_goroutines" is present in the scrape body regardless of which
// tracker subsystems have registered by the time this runs. Its presence
// is direct evidence that buildMetricsServer is wired to the real default
// registry — the same one broker/federation/reputation/admission/ledger
// register their collectors on in newRunCmd's RunE — and not a
// disconnected local registry.
func TestBuildMetricsServer_ServesMetricsUnauthenticated(t *testing.T) {
	cfg := &config.Config{Metrics: config.MetricsConfig{ListenAddr: "127.0.0.1:0"}}
	srv := buildMetricsServer(cfg)

	if srv.Addr != cfg.Metrics.ListenAddr {
		t.Fatalf("Addr = %q, want %q", srv.Addr, cfg.Metrics.ListenAddr)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	// Deliberately no Authorization header: /metrics must not require the
	// admin bearer token.
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "go_goroutines") {
		t.Fatalf("body missing go_goroutines: %s", rec.Body.String())
	}
}

// TestBuildMetricsServer_OnlyMetricsMounted confirms the listener exposes
// nothing but /metrics — no accidental catch-all route to the rest of the
// application surface.
func TestBuildMetricsServer_OnlyMetricsMounted(t *testing.T) {
	cfg := &config.Config{Metrics: config.MetricsConfig{ListenAddr: "127.0.0.1:0"}}
	srv := buildMetricsServer(cfg)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("expected non-200 for unmounted path, got %d", rec.Code)
	}
}

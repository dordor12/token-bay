package reputation

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/require"
)

func TestMetrics_RegisterAllExposed(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := newMetrics()
	require.NoError(t, m.register(reg))

	// Touch each metric so it appears in the snapshot. Counters and
	// histograms with no observations are emitted by promhttp anyway,
	// but observations make the test resilient to library changes.
	m.transitions.WithLabelValues("OK", "AUDIT", "zscore").Inc()
	m.breaches.WithLabelValues("invalid_proof_signature").Inc()
	m.skips.WithLabelValues("undersized_population").Inc()
	m.storageErrors.WithLabelValues("read").Inc()
	m.eventsIngested.WithLabelValues("network_requests_per_h").Inc()
	m.evaluatorPanics.Inc()
	m.cycleSeconds.Observe(0.01)
	m.score.Observe(0.5)
	m.state.WithLabelValues("OK").Set(1)

	h := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	h.ServeHTTP(rr, req)
	body, _ := io.ReadAll(rr.Body)
	text := string(body)

	for _, want := range []string{
		"reputation_state",
		"reputation_score",
		"reputation_transitions_total",
		"reputation_breach_total",
		"reputation_evaluator_cycle_seconds",
		"reputation_evaluator_skips_total",
		"reputation_storage_errors_total",
		"reputation_events_ingested_total",
		"reputation_evaluator_panics_total",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing metric %q in registry output", want)
		}
	}
}

package ledger

import "github.com/prometheus/client_golang/prometheus"

// IntegrityMetrics groups the Prometheus collectors that count
// AssertChainIntegrity invocations at runtime gates (startup, peer
// reconnect). One instance per process, owned by the composition root.
type IntegrityMetrics struct {
	ChecksTotal *prometheus.CounterVec
}

// NewIntegrityMetrics builds the collectors and registers them on r. A
// nil r skips registration so callers can opt out (tests, no-Prometheus
// builds). Result labels are documented as "pass" or "fail".
func NewIntegrityMetrics(r prometheus.Registerer) *IntegrityMetrics {
	m := &IntegrityMetrics{
		ChecksTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "token_bay_ledger_integrity_checks_total",
			Help: "AssertChainIntegrity invocations at runtime gates, by result.",
		}, []string{"result"}),
	}
	if r != nil {
		r.MustRegister(m.ChecksTotal)
	}
	return m
}

// RecordResult bumps the result-labeled counter. result is "pass" or
// "fail"; other labels are accepted but operator dashboards key on the
// two documented values.
func (m *IntegrityMetrics) RecordResult(result string) {
	if m == nil {
		return
	}
	m.ChecksTotal.WithLabelValues(result).Inc()
}

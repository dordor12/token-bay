package api

import "github.com/prometheus/client_golang/prometheus"

// PrometheusBootstrapMetrics is the production BootstrapPeersMetrics
// implementation. Constructed in cmd/token-bay-tracker against
// prometheus.DefaultRegisterer.
type PrometheusBootstrapMetrics struct {
	served prometheus.Counter
	errors *prometheus.CounterVec
	size   prometheus.Gauge
}

// NewPrometheusBootstrapMetrics registers the three metrics on reg and
// returns the impl. Calls MustRegister; failures are programming errors.
func NewPrometheusBootstrapMetrics(reg prometheus.Registerer) *PrometheusBootstrapMetrics {
	m := &PrometheusBootstrapMetrics{
		served: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tokenbay_tracker_bootstrap_peers_served_total",
			Help: "Total successful bootstrap-peers RPC responses.",
		}),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tokenbay_tracker_bootstrap_peers_errors_total",
			Help: "Bootstrap-peers RPC errors keyed by code.",
		}, []string{"code"}),
		size: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "tokenbay_tracker_bootstrap_peers_list_size",
			Help: "Size of the most recent bootstrap-peers list returned.",
		}),
	}
	reg.MustRegister(m.served, m.errors, m.size)
	return m
}

func (m *PrometheusBootstrapMetrics) IncBootstrapServed() { m.served.Inc() }
func (m *PrometheusBootstrapMetrics) IncBootstrapErrors(code string) {
	m.errors.WithLabelValues(code).Inc()
}
func (m *PrometheusBootstrapMetrics) SetBootstrapListSize(n int) { m.size.Set(float64(n)) }

var _ BootstrapPeersMetrics = (*PrometheusBootstrapMetrics)(nil)

package server

import "github.com/prometheus/client_golang/prometheus"

//nolint:unused
type udpMetrics struct {
	stunRequests   *prometheus.CounterVec
	turnDatagrams  *prometheus.CounterVec
	turnBytes      prometheus.Counter
	activeBindings prometheus.GaugeFunc
}

// newUDPMetrics builds the UDP data-plane collectors. activeBindingsFn samples
// the live relay binding count at scrape time.
//
//nolint:unused
func newUDPMetrics(activeBindingsFn func() float64) *udpMetrics {
	return &udpMetrics{
		stunRequests: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "tokenbay_stun_requests_total", Help: "STUN binding requests by outcome."},
			[]string{"outcome"}),
		turnDatagrams: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "tokenbay_turn_datagrams_total", Help: "TURN relay datagrams by outcome."},
			[]string{"outcome"}),
		turnBytes: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "tokenbay_turn_relayed_bytes_total", Help: "Total bytes forwarded by the TURN relay."}),
		activeBindings: prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{Name: "tokenbay_turn_active_bindings", Help: "Live TURN relay peer bindings."},
			activeBindingsFn),
	}
}

//nolint:unused
func (m *udpMetrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{m.stunRequests, m.turnDatagrams, m.turnBytes, m.activeBindings}
}

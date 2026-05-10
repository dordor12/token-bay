package federation

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the federation subsystem's Prometheus collector set.
// All metrics are prefixed tokenbay_federation_ per spec §12.
type Metrics struct {
	framesIn                  *prometheus.CounterVec
	framesOut                 *prometheus.CounterVec
	invalidFrames             *prometheus.CounterVec
	dedupeSize                prometheus.Gauge
	peers                     *prometheus.GaugeVec
	rootAttestationsPublished prometheus.Counter
	rootAttestationsReceived  *prometheus.CounterVec
	equivocationsDetected     prometheus.Counter
	equivocationsAboutSelf    prometheus.Counter
	revocationsEmitted        prometheus.Counter
	revocationsReceived       *prometheus.CounterVec
	peerExchangeEmitted       prometheus.Counter
	peerExchangeReceived      *prometheus.CounterVec
	knownPeersSize            prometheus.Gauge
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		framesIn:                  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "tokenbay_federation_frames_in_total"}, []string{"kind"}),
		framesOut:                 prometheus.NewCounterVec(prometheus.CounterOpts{Name: "tokenbay_federation_frames_out_total"}, []string{"kind"}),
		invalidFrames:             prometheus.NewCounterVec(prometheus.CounterOpts{Name: "tokenbay_federation_invalid_frames_total"}, []string{"reason"}),
		dedupeSize:                prometheus.NewGauge(prometheus.GaugeOpts{Name: "tokenbay_federation_dedupe_size"}),
		peers:                     prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "tokenbay_federation_peers"}, []string{"state"}),
		rootAttestationsPublished: prometheus.NewCounter(prometheus.CounterOpts{Name: "tokenbay_federation_root_attestations_published_total"}),
		rootAttestationsReceived:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "tokenbay_federation_root_attestations_received_total"}, []string{"outcome"}),
		equivocationsDetected:     prometheus.NewCounter(prometheus.CounterOpts{Name: "tokenbay_federation_equivocations_detected_total"}),
		equivocationsAboutSelf:    prometheus.NewCounter(prometheus.CounterOpts{Name: "tokenbay_federation_equivocations_about_self_total"}),
		revocationsEmitted:        prometheus.NewCounter(prometheus.CounterOpts{Name: "tokenbay_federation_revocations_emitted_total"}),
		revocationsReceived:       prometheus.NewCounterVec(prometheus.CounterOpts{Name: "tokenbay_federation_revocations_received_total"}, []string{"outcome"}),
		peerExchangeEmitted:       prometheus.NewCounter(prometheus.CounterOpts{Name: "tokenbay_federation_peer_exchange_emitted_total"}),
		peerExchangeReceived:      prometheus.NewCounterVec(prometheus.CounterOpts{Name: "tokenbay_federation_peer_exchange_received_total"}, []string{"outcome"}),
		knownPeersSize:            prometheus.NewGauge(prometheus.GaugeOpts{Name: "tokenbay_federation_known_peers_size"}),
	}
	for _, c := range []prometheus.Collector{m.framesIn, m.framesOut, m.invalidFrames, m.dedupeSize, m.peers, m.rootAttestationsPublished, m.rootAttestationsReceived, m.equivocationsDetected, m.equivocationsAboutSelf, m.revocationsEmitted, m.revocationsReceived, m.peerExchangeEmitted, m.peerExchangeReceived, m.knownPeersSize} {
		reg.MustRegister(c)
	}
	return m
}

func (m *Metrics) FramesIn(kind string)         { m.framesIn.WithLabelValues(kind).Inc() }
func (m *Metrics) FramesOut(kind string)        { m.framesOut.WithLabelValues(kind).Inc() }
func (m *Metrics) InvalidFrames(reason string)  { m.invalidFrames.WithLabelValues(reason).Inc() }
func (m *Metrics) SetDedupeSize(n int)          { m.dedupeSize.Set(float64(n)) }
func (m *Metrics) SetPeers(state string, n int) { m.peers.WithLabelValues(state).Set(float64(n)) }
func (m *Metrics) RootAttestationsPublished()   { m.rootAttestationsPublished.Inc() }
func (m *Metrics) RootAttestationsReceived(outcome string) {
	m.rootAttestationsReceived.WithLabelValues(outcome).Inc()
}
func (m *Metrics) EquivocationsDetected()  { m.equivocationsDetected.Inc() }
func (m *Metrics) EquivocationsAboutSelf() { m.equivocationsAboutSelf.Inc() }
func (m *Metrics) RevocationsEmitted()     { m.revocationsEmitted.Inc() }
func (m *Metrics) RevocationsReceived(outcome string) {
	m.revocationsReceived.WithLabelValues(outcome).Inc()
}

// Test-only accessors. Used by metrics_test.go via testutil.CollectAndCompare /
// testutil.ToFloat64. Not part of the production surface.
func (m *Metrics) FramesInVec() *prometheus.CounterVec { return m.framesIn }

func (m *Metrics) RootAttestationsPublishedCounter() prometheus.Counter {
	return m.rootAttestationsPublished
}

func (m *Metrics) EquivocationsDetectedCounter() prometheus.Counter {
	return m.equivocationsDetected
}

func (m *Metrics) RevocationsEmittedCounter() prometheus.Counter {
	return m.revocationsEmitted
}

func (m *Metrics) RevocationsReceivedVec() *prometheus.CounterVec {
	return m.revocationsReceived
}

func (m *Metrics) PeerExchangeEmitted() { m.peerExchangeEmitted.Inc() }
func (m *Metrics) PeerExchangeReceived(outcome string) {
	m.peerExchangeReceived.WithLabelValues(outcome).Inc()
}
func (m *Metrics) SetKnownPeersSize(n int) { m.knownPeersSize.Set(float64(n)) }

func (m *Metrics) PeerExchangeEmittedCounter() prometheus.Counter {
	return m.peerExchangeEmitted
}

func (m *Metrics) PeerExchangeReceivedVec() *prometheus.CounterVec {
	return m.peerExchangeReceived
}

func (m *Metrics) KnownPeersSizeGauge() prometheus.Gauge {
	return m.knownPeersSize
}

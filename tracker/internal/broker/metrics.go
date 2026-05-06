package broker

import "github.com/prometheus/client_golang/prometheus"

// brokerMetrics groups every counter/gauge/histogram exposed by the broker
// subsystem. One instance is allocated by newBrokerMetrics; fields are bumped
// directly on the hot path to avoid map lookups.
type brokerMetrics struct {
	// Hot-path
	SubmitDecisions *prometheus.CounterVec
	SubmitDuration  prometheus.Histogram
	OfferAttempts   *prometheus.CounterVec
	InflightCount   prometheus.Gauge

	// Reservations
	ReservationsActive      prometheus.Gauge
	ReservationsActiveCount prometheus.Gauge
	ReservationTTLExpired   prometheus.Counter

	// Settlement
	SettlementDecisions        *prometheus.CounterVec
	SettlementDuration         prometheus.Histogram
	ConsumerSigMissing         prometheus.Counter
	LedgerAppendFailure        prometheus.Counter
	StaleTipRetries            prometheus.Counter

	// Operational
	QueueDrainPops              prometheus.Counter
	QueueDrainAdmitOutcomes     *prometheus.CounterVec
	SeederPostAcceptDisconnect  prometheus.Counter
}

// newBrokerMetrics constructs all metrics. The returned struct must be
// registered with a prometheus.Registry (or prometheus.DefaultRegisterer) by
// the caller if Prometheus scraping is desired; the factory itself does not
// register so tests can call it without a live registry.
func newBrokerMetrics() *brokerMetrics {
	return &brokerMetrics{
		SubmitDecisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "broker_submit_decisions_total",
			Help: "Submit outcomes by result.",
		}, []string{"result"}),
		SubmitDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "broker_submit_duration_seconds",
			Help:    "End-to-end Submit latency distribution.",
			Buckets: prometheus.DefBuckets,
		}),
		OfferAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "broker_offer_attempts_total",
			Help: "Offer round-trips by outcome (accept/reject/timeout/unreachable).",
		}, []string{"outcome"}),
		InflightCount: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "broker_inflight_count",
			Help: "Current number of in-flight requests.",
		}),

		ReservationsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "broker_reservations_active",
			Help: "Sum of reserved credits across all active slots.",
		}),
		ReservationsActiveCount: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "broker_reservations_active_count",
			Help: "Number of active reservation slots.",
		}),
		ReservationTTLExpired: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "broker_reservation_ttl_expired_total",
			Help: "Cumulative reservation slots expired by the TTL reaper.",
		}),

		SettlementDecisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "broker_settlement_decisions_total",
			Help: "Settlement outcomes by result (ok/consumer_sig_missing/cost_overspend/sig_invalid/ledger_failed).",
		}, []string{"result"}),
		SettlementDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "broker_settlement_duration_seconds",
			Help:    "HandleUsageReport latency distribution.",
			Buckets: prometheus.DefBuckets,
		}),
		ConsumerSigMissing: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "broker_consumer_sig_missing_total",
			Help: "Cumulative settlements where no consumer counter-sig arrived before timeout.",
		}),
		LedgerAppendFailure: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "broker_ledger_append_failure_total",
			Help: "Cumulative ledger AppendUsage failures.",
		}),
		StaleTipRetries: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "broker_stale_tip_retries_total",
			Help: "Cumulative ErrStaleTip retries during ledger append.",
		}),

		QueueDrainPops: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "broker_queue_drain_pops_total",
			Help: "Cumulative entries popped from the pending queue by the drain loop.",
		}),
		QueueDrainAdmitOutcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "broker_queue_drain_admit_outcomes_total",
			Help: "Queue-drain re-submit outcomes by outcome label.",
		}, []string{"outcome"}),
		SeederPostAcceptDisconnect: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "broker_seeder_post_accept_disconnect_total",
			Help: "Cumulative seeder disconnects detected after offer acceptance.",
		}),
	}
}

// Collector returns a composite prometheus.Collector that wraps all broker
// metrics so they can be registered together with a single registry call.
func (m *brokerMetrics) Collector() prometheus.Collector {
	return &brokerCompositeCollector{m: m}
}

type brokerCompositeCollector struct {
	m *brokerMetrics
}

func (c *brokerCompositeCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, col := range c.m.collectors() {
		col.Describe(ch)
	}
}

func (c *brokerCompositeCollector) Collect(ch chan<- prometheus.Metric) {
	for _, col := range c.m.collectors() {
		col.Collect(ch)
	}
}

func (m *brokerMetrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.SubmitDecisions,
		m.SubmitDuration,
		m.OfferAttempts,
		m.InflightCount,
		m.ReservationsActive,
		m.ReservationsActiveCount,
		m.ReservationTTLExpired,
		m.SettlementDecisions,
		m.SettlementDuration,
		m.ConsumerSigMissing,
		m.LedgerAppendFailure,
		m.StaleTipRetries,
		m.QueueDrainPops,
		m.QueueDrainAdmitOutcomes,
		m.SeederPostAcceptDisconnect,
	}
}

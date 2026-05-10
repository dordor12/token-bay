package reputation

import "github.com/prometheus/client_golang/prometheus"

type metrics struct {
	state           *prometheus.GaugeVec
	score           prometheus.Histogram
	transitions     *prometheus.CounterVec
	breaches        *prometheus.CounterVec
	cycleSeconds    prometheus.Histogram
	skips           *prometheus.CounterVec
	storageErrors   *prometheus.CounterVec
	eventsIngested  *prometheus.CounterVec
	evaluatorPanics prometheus.Counter
}

func newMetrics() *metrics {
	return &metrics{
		state: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "reputation_state",
			Help: "Number of identities in each reputation state.",
		}, []string{"state"}),
		score: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "reputation_score",
			Help:    "Distribution of computed reputation scores.",
			Buckets: prometheus.LinearBuckets(0, 0.1, 11),
		}),
		transitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reputation_transitions_total",
			Help: "State transitions, labeled by from, to, and reason kind.",
		}, []string{"from", "to", "reason"}),
		breaches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reputation_breach_total",
			Help: "Categorical-breach events.",
		}, []string{"kind"}),
		cycleSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "reputation_evaluator_cycle_seconds",
			Help:    "Wall-time of one evaluator cycle.",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 12),
		}),
		skips: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reputation_evaluator_skips_total",
			Help: "Evaluator skip events labeled by reason.",
		}, []string{"reason"}),
		storageErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reputation_storage_errors_total",
			Help: "Storage errors by op (read|write|migrate).",
		}, []string{"op"}),
		eventsIngested: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "reputation_events_ingested_total",
			Help: "Events ingested per signal kind.",
		}, []string{"event_type"}),
		evaluatorPanics: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "reputation_evaluator_panics_total",
			Help: "Recovered evaluator panics.",
		}),
	}
}

func (m *metrics) register(r prometheus.Registerer) error {
	for _, c := range []prometheus.Collector{
		m.state, m.score, m.transitions, m.breaches, m.cycleSeconds,
		m.skips, m.storageErrors, m.eventsIngested, m.evaluatorPanics,
	} {
		if err := r.Register(c); err != nil {
			return err
		}
	}
	return nil
}

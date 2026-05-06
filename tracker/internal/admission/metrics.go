package admission

import (
	"github.com/prometheus/client_golang/prometheus"
)

// admissionMetrics groups every counter/gauge/histogram exposed by the
// subsystem's Collector. One instance per Subsystem; the Subsystem holds
// pointers so Decide / IssueAttestation / replay can bump without an
// extra map lookup.
type admissionMetrics struct {
	Decisions          *prometheus.CounterVec
	QueueDepth         prometheus.Gauge
	Pressure           prometheus.Gauge
	SupplyTotal        prometheus.Gauge
	DemandRate         prometheus.Gauge
	DecisionDuration   prometheus.Histogram
	AttestIssued       prometheus.Counter
	AttestValFails     *prometheus.CounterVec
	AttestAge          prometheus.Histogram
	TrialTierDecisions prometheus.Counter
	TLogReplayGap      prometheus.Gauge
	TLogCorruptions    prometheus.Counter
	SnapshotLoadFails  prometheus.Counter
	SnapshotEmitFails  prometheus.Counter
	ClockJumps         *prometheus.CounterVec
	FetchHeadroomToS   prometheus.Counter
	RejectionsByReason *prometheus.CounterVec
	PressureCrossings  *prometheus.CounterVec
	SeedersContrib     prometheus.Gauge
}

func newAdmissionMetrics() *admissionMetrics {
	return &admissionMetrics{
		Decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "admission_decisions_total",
			Help: "Decide outcomes by result label.",
		}, []string{"result"}),
		QueueDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "admission_queue_depth",
			Help: "Current admission queue size.",
		}),
		Pressure: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "admission_pressure",
			Help: "Current pressure ratio (demand/supply).",
		}),
		SupplyTotal: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "admission_supply_total_headroom",
			Help: "Sum of contributing-seeder headroom estimates.",
		}),
		DemandRate: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "admission_demand_rate_ewma",
			Help: "Exponentially-weighted moving average of broker_request rate.",
		}),
		DecisionDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "admission_decision_duration_seconds",
			Help:    "Decide latency distribution.",
			Buckets: prometheus.DefBuckets,
		}),
		AttestIssued: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "admission_attestations_issued_total",
			Help: "Cumulative attestations issued.",
		}),
		AttestValFails: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "admission_attestation_validation_failures_total",
			Help: "Imported attestation rejections by stage.",
		}, []string{"stage"}),
		AttestAge: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "admission_attestation_age_seconds",
			Help:    "Time-since-issued at validation.",
			Buckets: []float64{1, 60, 600, 3600, 21600, 86400},
		}),
		TrialTierDecisions: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "admission_trial_tier_decisions_total",
			Help: "Decisions that fell back to TrialTierScore.",
		}),
		TLogReplayGap: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "admission_tlog_replay_gap_entries",
			Help: "Ledger entries applied during cross-check that were missing from tlog.",
		}),
		TLogCorruptions: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "admission_tlog_corruption_records_total",
			Help: "Cumulative tlog records that failed CRC mid-file.",
		}),
		SnapshotLoadFails: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "admission_snapshot_load_failures_total",
			Help: "Cumulative failed snapshot loads (any reason).",
		}),
		SnapshotEmitFails: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "admission_snapshot_emit_failures_total",
			Help: "Cumulative failed snapshot emit attempts.",
		}),
		ClockJumps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "admission_clock_jump_detected_total",
			Help: "Detected wall-clock jumps by direction.",
		}, []string{"direction"}),
		FetchHeadroomToS: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "admission_fetchheadroom_timeouts_total",
			Help: "FetchHeadroom RPC timeouts (production wiring lands later).",
		}),
		RejectionsByReason: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "admission_rejections_total",
			Help: "Reject decisions by reason.",
		}, []string{"reason"}),
		PressureCrossings: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "admission_pressure_threshold_crossing_total",
			Help: "Pressure threshold crossings by direction.",
		}, []string{"direction"}),
		SeedersContrib: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "admission_seeders_contributing",
			Help: "Number of seeders contributing to the latest supply snapshot.",
		}),
	}
}

// dynamicCollector emits gauges whose values are read from atomic state
// at scrape time. Avoids double-bookkeeping for things already tracked
// elsewhere (DegradedMode, snapshot/tlog counters).
type dynamicCollector struct {
	s              *Subsystem
	degradedDesc   *prometheus.Desc
	queueDepthDesc *prometheus.Desc
}

func newDynamicCollector(s *Subsystem) *dynamicCollector {
	return &dynamicCollector{
		s: s,
		degradedDesc: prometheus.NewDesc(
			"admission_degraded_mode_active",
			"1 when admission is running without a usable snapshot. 0 otherwise.",
			nil, nil,
		),
		queueDepthDesc: prometheus.NewDesc(
			"admission_queue_depth_live",
			"Live queue depth at scrape time (mirror of admission_queue_depth).",
			nil, nil,
		),
	}
}

func (c *dynamicCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.degradedDesc
	ch <- c.queueDepthDesc
}

func (c *dynamicCollector) Collect(ch chan<- prometheus.Metric) {
	val := 0.0
	if c.s.DegradedMode() {
		val = 1.0
	}
	ch <- prometheus.MustNewConstMetric(c.degradedDesc, prometheus.GaugeValue, val)
	ch <- prometheus.MustNewConstMetric(c.queueDepthDesc, prometheus.GaugeValue, float64(c.s.queue.Len()))
}

// Collector returns one composite prometheus.Collector wrapping every
// admission metric.
func (s *Subsystem) Collector() prometheus.Collector {
	return &compositeCollector{
		subs: []prometheus.Collector{
			s.metrics.Decisions,
			s.metrics.QueueDepth,
			s.metrics.Pressure,
			s.metrics.SupplyTotal,
			s.metrics.DemandRate,
			s.metrics.DecisionDuration,
			s.metrics.AttestIssued,
			s.metrics.AttestValFails,
			s.metrics.AttestAge,
			s.metrics.TrialTierDecisions,
			s.metrics.TLogReplayGap,
			s.metrics.TLogCorruptions,
			s.metrics.SnapshotLoadFails,
			s.metrics.SnapshotEmitFails,
			s.metrics.ClockJumps,
			s.metrics.FetchHeadroomToS,
			s.metrics.RejectionsByReason,
			s.metrics.PressureCrossings,
			s.metrics.SeedersContrib,
			newDynamicCollector(s),
		},
	}
}

type compositeCollector struct {
	subs []prometheus.Collector
}

func (c *compositeCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, sc := range c.subs {
		sc.Describe(ch)
	}
}

func (c *compositeCollector) Collect(ch chan<- prometheus.Metric) {
	for _, sc := range c.subs {
		sc.Collect(ch)
	}
}

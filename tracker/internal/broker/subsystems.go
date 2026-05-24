package broker

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/session"
)

// Subsystems is the composite that ties *Broker and *Settlement to a shared
// session.Manager (Inflight + Reservations). Construct via Open; the two
// subsystems are accessible as fields for api/ wiring.
type Subsystems struct {
	Broker     *Broker
	Settlement *Settlement
	metrics    *brokerMetrics
}

// Open constructs both subsystems sharing one session.Manager. Goroutine
// ownership is split: queue-drain lives with Broker, reservation TTL reaper
// lives with Settlement. Close() shuts both down in dependency order.
func Open(cfg config.BrokerConfig, scfg config.SettlementConfig, deps Deps) (*Subsystems, error) {
	mgr := session.New()
	metrics := newBrokerMetrics()
	b, err := openBrokerWithMetrics(cfg, scfg, deps, mgr, metrics)
	if err != nil {
		return nil, err
	}
	s, err := openSettlementWithMetrics(scfg, deps, mgr, metrics)
	if err != nil {
		_ = b.Close()
		return nil, err
	}
	return &Subsystems{Broker: b, Settlement: s, metrics: metrics}, nil
}

// Close shuts down both subsystems in dependency order. Idempotent.
func (s *Subsystems) Close() error {
	err1 := s.Broker.Close()
	err2 := s.Settlement.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// Collector returns a prometheus.Collector wrapping every broker/settlement
// metric so callers can register the pair with one call. Includes a dynamic
// gauge for the pending-queue depth (pattern from
// tracker/internal/admission/metrics.go).
func (s *Subsystems) Collector() prometheus.Collector {
	return &subsystemsCollector{m: s.metrics, base: s.metrics.Collector(), s: s}
}

var pendingQueueDepthDesc = prometheus.NewDesc(
	"broker_pending_queue_depth",
	"Number of RegisterQueued entries currently waiting for drain.",
	nil, nil,
)

type subsystemsCollector struct {
	m    *brokerMetrics
	base prometheus.Collector
	s    *Subsystems
}

func (c *subsystemsCollector) Describe(ch chan<- *prometheus.Desc) {
	c.base.Describe(ch)
	ch <- pendingQueueDepthDesc
}

func (c *subsystemsCollector) Collect(ch chan<- prometheus.Metric) {
	c.base.Collect(ch)
	ch <- prometheus.MustNewConstMetric(
		pendingQueueDepthDesc, prometheus.GaugeValue,
		float64(c.s.Broker.pendingQueueLen()),
	)
}

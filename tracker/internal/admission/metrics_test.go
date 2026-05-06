package admission

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func TestMetrics_DecisionCounterBumps(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 0.5})

	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(s.Collector()))

	for i := 0; i < 3; i++ {
		s.Decide(makeIDi(i), nil, now)
	}
	expected := `
# HELP admission_decisions_total Decide outcomes by result label.
# TYPE admission_decisions_total counter
admission_decisions_total{result="admit"} 3
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"admission_decisions_total"))
}

func TestMetrics_DegradedGaugeReflectsState(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	s.degradedMode.Store(1)

	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(s.Collector()))

	expected := `
# HELP admission_degraded_mode_active 1 when admission is running without a usable snapshot. 0 otherwise.
# TYPE admission_degraded_mode_active gauge
admission_degraded_mode_active 1
`
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"admission_degraded_mode_active"))
}

func TestMetrics_QueueDepthGauge(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 1.0})
	for i := 0; i < 4; i++ {
		s.Decide(makeIDi(i), nil, now)
	}
	got := testutil.ToFloat64(s.metrics.QueueDepth)
	require.InDelta(t, 4.0, got, 0)
}

func TestMetrics_RejectionsCounterByReason(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 2.0})
	for i := 0; i < 2; i++ {
		s.Decide(makeIDi(i), nil, now)
	}
	got := testutil.ToFloat64(s.metrics.RejectionsByReason.WithLabelValues("region_overloaded"))
	require.InDelta(t, 2.0, got, 0)
}

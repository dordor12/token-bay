package reputation

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/tracker/internal/config"
)

// frozenClock is a deterministic clock for the evaluator tests.
type frozenClock struct{ t time.Time }

func (c *frozenClock) Now() time.Time      { return c.t }
func (c *frozenClock) Add(d time.Duration) { c.t = c.t.Add(d) }

func openForEvaluatorTest(t *testing.T, clk *frozenClock) *Subsystem {
	t.Helper()
	cfg := config.DefaultConfig().Reputation
	cfg.StoragePath = filepath.Join(t.TempDir(), "rep.sqlite")
	cfg.MinPopulationForZScore = 5
	cfg.EvaluationIntervalS = 60
	cfg.ZScoreThreshold = 2.5
	s, err := Open(context.Background(), cfg, WithClock(clk.Now))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestMedianMAD_Empty(t *testing.T) {
	median, mad := medianMAD(nil)
	require.Equal(t, 0.0, median)
	require.Equal(t, 0.0, mad)
}

func TestMedianMAD_Single(t *testing.T) {
	median, mad := medianMAD([]float64{3.0})
	require.InDelta(t, 3.0, median, 0.0001)
	require.InDelta(t, 0.0, mad, 0.0001)
}

func TestMedianMAD_OddCount(t *testing.T) {
	median, mad := medianMAD([]float64{1, 2, 3, 4, 5})
	require.InDelta(t, 3.0, median, 0.0001)
	// |xi - median| = [2,1,0,1,2] → MAD median = 1.0.
	require.InDelta(t, 1.0, mad, 0.0001)
}

func TestMedianMAD_EvenCount(t *testing.T) {
	median, mad := medianMAD([]float64{1, 2, 3, 4})
	require.InDelta(t, 2.5, median, 0.0001)
	// |xi - 2.5| = [1.5, 0.5, 0.5, 1.5] → MAD median = 1.0.
	require.InDelta(t, 1.0, mad, 0.0001)
}

func TestZScore_ZeroMAD(t *testing.T) {
	// All samples equal → MAD = 0; z must be defined-as-zero, not Inf.
	require.Equal(t, 0.0, zScore(5.0, 5.0, 0.0))
}

func TestZScore_NormalCase(t *testing.T) {
	z := zScore(7.0, 3.0, 2.0)
	require.InDelta(t, 2.0, z, 0.0001)
}

func TestZScore_NegativeOutlier(t *testing.T) {
	z := zScore(-1.0, 3.0, 2.0)
	if !math.Signbit(z) {
		t.Errorf("expected negative z, got %f", z)
	}
}

func TestEvaluator_OutlierTriggersAudit(t *testing.T) {
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	s := openForEvaluatorTest(t, clk)
	ctx := context.Background()

	// Use varied counts (1..10) so MAD > 0. Identity i has i+1 events.
	for i := byte(0); i < 10; i++ {
		for j := byte(0); j <= i; j++ {
			require.NoError(t, s.RecordBrokerRequest(mkID(0xA0+i), "admit"))
		}
	}
	outlier := mkID(0xCC)
	for i := 0; i < 100; i++ {
		require.NoError(t, s.RecordBrokerRequest(outlier, "admit"))
	}

	require.NoError(t, s.runOneCycle(ctx))

	st := s.Status(outlier)
	require.Equal(t, StateAudit, st.State,
		"high-outlier consumer should be in AUDIT")
	require.NotEmpty(t, st.Reasons)
	require.Equal(t, "zscore", st.Reasons[0].Kind)
}

func TestEvaluator_BootstrapBelowMinPopSkipsZScore(t *testing.T) {
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	s := openForEvaluatorTest(t, clk)
	ctx := context.Background()

	for i := byte(0); i < 3; i++ {
		require.NoError(t, s.RecordBrokerRequest(mkID(0xD0|i), "admit"))
	}
	outlier := mkID(0xEE)
	for i := 0; i < 100; i++ {
		require.NoError(t, s.RecordBrokerRequest(outlier, "admit"))
	}

	require.NoError(t, s.runOneCycle(ctx))
	require.Equal(t, StateOK, s.Status(outlier).State,
		"below MinPop, z-score must be skipped")
}

func TestEvaluator_ThreeAuditsInSevenDaysFreezes(t *testing.T) {
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	s := openForEvaluatorTest(t, clk)
	id := mkID(0xF0)

	for i := 0; i < 3; i++ {
		require.NoError(t,
			s.RecordCategoricalBreach(id, BreachInvalidProofSignature))
		clk.Add(24 * time.Hour)
		require.NoError(t, s.runOneCycle(context.Background()))
	}
	require.Equal(t, StateFrozen, s.Status(id).State)
	require.True(t, s.IsFrozen(id))
}

func TestEvaluator_AuditClearsAfter48hOfClean(t *testing.T) {
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	s := openForEvaluatorTest(t, clk)
	id := mkID(0xAB)
	require.NoError(t,
		s.RecordCategoricalBreach(id, BreachInvalidProofSignature))
	require.Equal(t, StateAudit, s.Status(id).State)

	clk.Add(49 * time.Hour)
	require.NoError(t, s.runOneCycle(context.Background()))
	require.Equal(t, StateOK, s.Status(id).State,
		"AUDIT should clear after 48h of clean")
}

func TestEvaluator_PrunesEventsPastLongWindow(t *testing.T) {
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	s := openForEvaluatorTest(t, clk)
	id := mkID(0xBC)

	require.NoError(t, s.RecordBrokerRequest(id, "admit"))
	clk.Add(8 * 24 * time.Hour) // 8 days; long window default 7
	require.NoError(t, s.runOneCycle(context.Background()))

	n := countEvents(t, s, SignalBrokerRequest)
	require.Equal(t, 0, n, "events older than long-window must be pruned")
}

package reputation

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/admission"
	"github.com/token-bay/token-bay/tracker/internal/config"
)

// TestIntegration_FullCycle exercises the spec §11 acceptance criteria
// against a real SQLite DB at t.TempDir(): bootstrap-gated z-score
// detection, score-better-than-default for a normal consumer, and
// 3-breach-in-7d freeze.
func TestIntegration_FullCycle(t *testing.T) {
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	cfg := config.DefaultConfig().Reputation
	cfg.StoragePath = filepath.Join(t.TempDir(), "rep.sqlite")
	cfg.MinPopulationForZScore = 10
	cfg.EvaluationIntervalS = 60
	cfg.ZScoreThreshold = 2.5

	s, err := Open(context.Background(), cfg, WithClock(clk.Now))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()

	// 100 normal consumers with varied request counts so MAD > 0;
	// 1 outlier with 200 requests.
	var ids100 [100]ids.IdentityID
	for i := range ids100 {
		ids100[i] = mkID(byte(i))
		// Vary count from 1 to 5 so MAD > 0.
		count := (i % 5) + 1
		for j := 0; j < count; j++ {
			require.NoError(t, s.RecordBrokerRequest(ids100[i], "admit"))
		}
	}
	outlier := mkID(0xCC)
	for i := 0; i < 200; i++ {
		require.NoError(t, s.RecordBrokerRequest(outlier, "admit"))
	}

	// 12 settlements forwarding from the broker.
	var seeders [12]ids.IdentityID
	for i := range seeders {
		seeders[i] = mkID(byte(0x80 + i))
		s.OnLedgerEvent(admission.LedgerEvent{
			Kind:        admission.LedgerEventSettlement,
			ConsumerID:  ids100[i],
			SeederID:    seeders[i],
			CostCredits: 1000,
			Timestamp:   clk.Now(),
		})
	}

	// Cycle 1: outlier should land in AUDIT via z-score.
	require.NoError(t, s.runOneCycle(ctx))
	require.Equal(t, StateAudit, s.Status(outlier).State)

	// Score for a normal consumer should be > default after cycle 1
	// (longevity bonus is ~0 at t=0d, but no audit penalty either, so
	// the score equals the default-input score of 0.8 > 0.5).
	score, ok := s.Score(ids100[0])
	require.True(t, ok)
	require.Greater(t, score, cfg.DefaultScore)

	// Two more breaches in 7d → FROZEN.
	require.NoError(t,
		s.RecordCategoricalBreach(outlier, BreachInvalidProofSignature))
	clk.Add(2 * 24 * time.Hour)
	require.NoError(t, s.runOneCycle(ctx))

	require.NoError(t,
		s.RecordCategoricalBreach(outlier, BreachInvalidProofSignature))
	clk.Add(2 * 24 * time.Hour)
	require.NoError(t, s.runOneCycle(ctx))

	require.Equal(t, StateFrozen, s.Status(outlier).State)
	require.True(t, s.IsFrozen(outlier))
}

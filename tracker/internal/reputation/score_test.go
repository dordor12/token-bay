package reputation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSubsystemScore_CacheMissReturnsDefault(t *testing.T) {
	s := openForTest(t)
	score, ok := s.Score(mkID(0xFF))
	require.False(t, ok)
	require.InDelta(t, s.cfg.DefaultScore, score, 0.0001)
}

func TestSubsystemIsFrozen_CacheMissReturnsFalse(t *testing.T) {
	s := openForTest(t)
	require.False(t, s.IsFrozen(mkID(0xEE)))
}

func TestSubsystemStatus_CacheMissReturnsZero(t *testing.T) {
	s := openForTest(t)
	st := s.Status(mkID(0xDD))
	require.Equal(t, StateOK, st.State)
	require.True(t, st.Since.IsZero())
	require.Empty(t, st.Reasons)
}

func TestSubsystemScore_CacheHit(t *testing.T) {
	s := openForTest(t)
	id := mkID(0x42)
	now := time.Unix(1_700_000_000, 0)
	require.NoError(t, s.store.ensureState(t.Context(), id, now))
	require.NoError(t, s.store.upsertScore(t.Context(), id, 0.7, now))
	require.NoError(t, s.reloadCache(t.Context()))

	score, ok := s.Score(id)
	require.True(t, ok)
	require.InDelta(t, 0.7, score, 0.0001)
	require.False(t, s.IsFrozen(id))
}

func TestSubsystemIsFrozen_TrueForFrozenState(t *testing.T) {
	s := openForTest(t)
	id := mkID(0x99)
	now := time.Unix(1_700_000_000, 0)
	require.NoError(t, s.store.ensureState(t.Context(), id, now))
	require.NoError(t, s.store.transition(t.Context(), id, StateAudit,
		ReasonRecord{
			Kind: "breach", BreachKind: "invalid_proof_signature",
			At: now.Unix(),
		}, now))
	require.NoError(t, s.store.transition(t.Context(), id, StateFrozen,
		ReasonRecord{
			Kind: "breach", BreachKind: "invalid_proof_signature",
			At: now.Unix(),
		}, now))
	require.NoError(t, s.reloadCache(t.Context()))

	require.True(t, s.IsFrozen(id))
	st := s.Status(id)
	require.Equal(t, StateFrozen, st.State)
	require.Len(t, st.Reasons, 2)
}

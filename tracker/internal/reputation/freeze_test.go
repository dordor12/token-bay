package reputation

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFreeze_SetsStateFrozenAndFiresRevocation covers the operator lever
// this task adds: a real z-score-driven FROZEN transition needs >=3
// zscore/breach reasons inside a 7-day window, which external test
// traffic can't produce in test time, so e2e revocation-propagation
// scenarios need this deterministic path instead.
func TestFreeze_SetsStateFrozenAndFiresRevocation(t *testing.T) {
	listener := &fakeFreezeListener{}
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	s := openForListenerTest(t, clk, WithFreezeListener(listener))

	id := mkID(0xA1)
	require.NoError(t, s.Freeze(context.Background(), id, "alice@example.com"))

	require.True(t, s.IsFrozen(id), "IsFrozen must reflect Freeze after refreshOne")
	require.Equal(t, StateFrozen, s.Status(id).State)

	calls := listener.snapshot()
	require.Len(t, calls, 1, "Freeze must gossip the REVOCATION via notifyFreeze")
	assert.Equal(t, id, calls[0].id)
	assert.Equal(t, "operator", calls[0].reason)
	assert.Equal(t, clk.Now(), calls[0].revokedAt)
}

// TestUnfreeze_ReturnsStateToOK covers the canTransition bypass: FROZEN
// is terminal for every automatic path, so Unfreeze must go through
// storage.clearFrozen to leave it. No revocation should fire for the
// unfreeze — v1 does not un-gossip a REVOCATION.
func TestUnfreeze_ReturnsStateToOK(t *testing.T) {
	listener := &fakeFreezeListener{}
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	s := openForListenerTest(t, clk, WithFreezeListener(listener))

	id := mkID(0xA2)
	require.NoError(t, s.Freeze(context.Background(), id, "alice@example.com"))
	require.True(t, s.IsFrozen(id))

	clk.Add(time.Minute)
	require.NoError(t, s.Unfreeze(context.Background(), id, "bob@example.com"))

	require.False(t, s.IsFrozen(id), "IsFrozen must reflect Unfreeze after refreshOne")
	require.Equal(t, StateOK, s.Status(id).State)

	calls := listener.snapshot()
	require.Len(t, calls, 1, "Unfreeze must NOT fire another OnFreeze/revocation")
}

// TestFreezeUnfreeze_ReasonsAppendOnly asserts the append-only invariant
// on rep_state.reasons: after Freeze then Unfreeze, both reasons are
// present and the freeze reason is untouched by the later Unfreeze.
func TestFreezeUnfreeze_ReasonsAppendOnly(t *testing.T) {
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	s := openForListenerTest(t, clk)

	id := mkID(0xA3)
	require.NoError(t, s.Freeze(context.Background(), id, "alice@example.com"))

	st := s.Status(id)
	require.Len(t, st.Reasons, 1)
	freezeReason := st.Reasons[0]
	assert.Equal(t, "manual", freezeReason.Kind)
	assert.Equal(t, "alice@example.com", freezeReason.Operator)

	clk.Add(time.Minute)
	require.NoError(t, s.Unfreeze(context.Background(), id, "bob@example.com"))

	st = s.Status(id)
	require.Len(t, st.Reasons, 2, "reasons must grow, not shrink or rewrite")
	assert.Equal(t, freezeReason, st.Reasons[0], "freeze reason must not be rewritten")
	assert.Equal(t, "manual", st.Reasons[1].Kind)
	assert.Equal(t, "bob@example.com", st.Reasons[1].Operator)
}

func TestFreeze_OnClosedSubsystemReturnsError(t *testing.T) {
	s := openForTest(t)
	require.NoError(t, s.Close())
	require.ErrorIs(t,
		s.Freeze(context.Background(), mkID(0xA4), "alice@example.com"),
		ErrSubsystemClosed)
}

func TestUnfreeze_OnClosedSubsystemReturnsError(t *testing.T) {
	s := openForTest(t)
	require.NoError(t, s.Close())
	require.ErrorIs(t,
		s.Unfreeze(context.Background(), mkID(0xA5), "alice@example.com"),
		ErrSubsystemClosed)
}

func TestFreeze_AlreadyFrozenIsNoOpNoDoubleRevocation(t *testing.T) {
	listener := &fakeFreezeListener{}
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	s := openForListenerTest(t, clk, WithFreezeListener(listener))

	id := mkID(0xA6)
	require.NoError(t, s.Freeze(context.Background(), id, "alice@example.com"))
	require.NoError(t, s.Freeze(context.Background(), id, "alice@example.com"))

	require.True(t, s.IsFrozen(id))
	calls := listener.snapshot()
	require.Len(t, calls, 1, "re-freezing an already-FROZEN identity must not re-gossip")
	require.Len(t, s.Status(id).Reasons, 1, "no duplicate reason for a no-op re-freeze")
}

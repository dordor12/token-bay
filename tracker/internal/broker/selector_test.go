package broker

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/registry"
)

func TestPick_FiltersByModel(t *testing.T) {
	snap := []registry.SeederRecord{
		seederRecord(t, ids.IdentityID{1}, 0.5, "claude-opus-4-7"),
		seederRecord(t, ids.IdentityID{2}, 0.5, "claude-sonnet-4-6"),
	}
	cands := Pick(snap, &tbproto.EnvelopeBody{
		Model: "claude-opus-4-7",
		Tier:  tbproto.PrivacyTier_PRIVACY_TIER_STANDARD,
	}, defaultWeights(), newStubReputation(), defaultBrokerCfg(), nil)
	require.Len(t, cands, 1)
	require.Equal(t, ids.IdentityID{1}, cands[0].Record.IdentityID)
}

func TestPick_ScoreOrder(t *testing.T) {
	snap := []registry.SeederRecord{
		seederRecord(t, ids.IdentityID{1}, 0.9, "x"),
		seederRecord(t, ids.IdentityID{2}, 0.3, "x"),
	}
	env := &tbproto.EnvelopeBody{Model: "x", Tier: tbproto.PrivacyTier_PRIVACY_TIER_STANDARD}
	cands := Pick(snap, env, defaultWeights(), newStubReputation(), defaultBrokerCfg(), nil)
	require.Len(t, cands, 2)
	require.Equal(t, ids.IdentityID{1}, cands[0].Record.IdentityID) // higher headroom wins
}

func TestPick_ExcludesAlreadyTried(t *testing.T) {
	snap := []registry.SeederRecord{
		seederRecord(t, ids.IdentityID{1}, 0.9, "x"),
		seederRecord(t, ids.IdentityID{2}, 0.5, "x"),
	}
	env := &tbproto.EnvelopeBody{Model: "x", Tier: tbproto.PrivacyTier_PRIVACY_TIER_STANDARD}
	cands := Pick(snap, env, defaultWeights(), newStubReputation(), defaultBrokerCfg(), []ids.IdentityID{{1}})
	require.Len(t, cands, 1)
	require.Equal(t, ids.IdentityID{2}, cands[0].Record.IdentityID)
}

func TestPick_FrozenSeederExcluded(t *testing.T) {
	snap := []registry.SeederRecord{
		seederRecord(t, ids.IdentityID{1}, 0.9, "x"),
		seederRecord(t, ids.IdentityID{2}, 0.5, "x"),
	}
	rep := newStubReputation()
	rep.frozen[ids.IdentityID{1}] = true
	env := &tbproto.EnvelopeBody{Model: "x", Tier: tbproto.PrivacyTier_PRIVACY_TIER_STANDARD}
	cands := Pick(snap, env, defaultWeights(), rep, defaultBrokerCfg(), nil)
	require.Len(t, cands, 1)
	require.Equal(t, ids.IdentityID{2}, cands[0].Record.IdentityID)
}

func TestPick_LoadCeiling(t *testing.T) {
	rec := seederRecord(t, ids.IdentityID{1}, 0.9, "x")
	rec.Load = 5
	snap := []registry.SeederRecord{rec}
	env := &tbproto.EnvelopeBody{Model: "x", Tier: tbproto.PrivacyTier_PRIVACY_TIER_STANDARD}
	cands := Pick(snap, env, defaultWeights(), newStubReputation(), defaultBrokerCfg(), nil)
	require.Empty(t, cands)
}

func TestPick_HeadroomFloor(t *testing.T) {
	rec := seederRecord(t, ids.IdentityID{1}, 0.1, "x") // below 0.2 default floor
	snap := []registry.SeederRecord{rec}
	env := &tbproto.EnvelopeBody{Model: "x", Tier: tbproto.PrivacyTier_PRIVACY_TIER_STANDARD}
	cands := Pick(snap, env, defaultWeights(), newStubReputation(), defaultBrokerCfg(), nil)
	require.Empty(t, cands)
}

func TestPick_TieBreakLex(t *testing.T) {
	snap := []registry.SeederRecord{
		seederRecord(t, ids.IdentityID{2}, 0.5, "x"),
		seederRecord(t, ids.IdentityID{1}, 0.5, "x"),
	}
	env := &tbproto.EnvelopeBody{Model: "x", Tier: tbproto.PrivacyTier_PRIVACY_TIER_STANDARD}
	cands := Pick(snap, env, defaultWeights(), newStubReputation(), defaultBrokerCfg(), nil)
	require.Len(t, cands, 2)
	// Equal scores → smaller IdentityID wins:
	require.Equal(t, ids.IdentityID{1}, cands[0].Record.IdentityID)
}

func TestPick_UnavailableExcluded(t *testing.T) {
	rec := seederRecord(t, ids.IdentityID{1}, 0.9, "x")
	rec.Available = false
	snap := []registry.SeederRecord{rec}
	env := &tbproto.EnvelopeBody{Model: "x", Tier: tbproto.PrivacyTier_PRIVACY_TIER_STANDARD}
	cands := Pick(snap, env, defaultWeights(), newStubReputation(), defaultBrokerCfg(), nil)
	require.Empty(t, cands)
}

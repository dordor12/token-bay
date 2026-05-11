package broker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
)

// Reputation §10 acceptance row 3: "Frozen identity's broker_request
// returns IDENTITY_FROZEN." The reputation subsystem exposes IsFrozen;
// broker.Submit must consult it on the consumer before reserving credits
// — independent of the cross-region RevocationArchive (which only
// covers federation-gossiped peer revocations).

func TestSubmit_RejectsLocallyFrozenConsumer(t *testing.T) {
	deps := testDeps(t)
	fr := newFakeRegistry()
	fr.Add(seederRecord(t, ids.IdentityID{1}, 0.9, "claude-sonnet-4-6"))
	deps.Registry = fr
	p := withFakePusher(t, &deps)
	// Queue an offer decision so the test would otherwise admit; the
	// local-freeze pre-check must short-circuit before any offer.
	queueDecision(p, true, bytesAllB(32, 0xCC))

	rep := newStubReputation()
	deps.Reputation = rep
	// RevocationArchive intentionally left nil: this case is purely
	// about the local IsFrozen gate.

	b, err := OpenBroker(defaultBrokerCfg(), testSettlementCfg(), deps, nil)
	require.NoError(t, err)
	defer b.Close()

	env := makeAdmittedEnvelope(t, "claude-sonnet-4-6", 100, 200, 1_000_000)
	var consumer ids.IdentityID
	copy(consumer[:], env.Body.ConsumerId)
	rep.frozen[consumer] = true

	_, err = b.Submit(context.Background(), env)
	require.ErrorIs(t, err, ErrIdentityFrozen)

	// The check must run before any reservation is held.
	require.Equal(t, uint64(0), b.mgr.Reservations.Reserved(consumer))
}

func TestSubmit_RejectsLocallyFrozenConsumer_EvenWithCleanArchive(t *testing.T) {
	deps := testDeps(t)
	fr := newFakeRegistry()
	fr.Add(seederRecord(t, ids.IdentityID{1}, 0.9, "claude-sonnet-4-6"))
	deps.Registry = fr
	p := withFakePusher(t, &deps)
	queueDecision(p, true, bytesAllB(32, 0xCC))

	rep := newStubReputation()
	deps.Reputation = rep
	arch := newStubRevocationArchive() // empty — no cross-region revocation
	deps.RevocationArchive = arch

	b, err := OpenBroker(defaultBrokerCfg(), testSettlementCfg(), deps, nil)
	require.NoError(t, err)
	defer b.Close()

	env := makeAdmittedEnvelope(t, "claude-sonnet-4-6", 100, 200, 1_000_000)
	var consumer ids.IdentityID
	copy(consumer[:], env.Body.ConsumerId)
	rep.frozen[consumer] = true

	_, err = b.Submit(context.Background(), env)
	require.ErrorIs(t, err, ErrIdentityFrozen)
	require.Equal(t, uint64(0), b.mgr.Reservations.Reserved(consumer))
}

func TestSubmit_LocalFrozen_AllowsOtherConsumer(t *testing.T) {
	// Freezing one consumer must not affect a different consumer.
	deps := testDeps(t)
	fr := newFakeRegistry()
	fr.Add(seederRecord(t, ids.IdentityID{1}, 0.9, "claude-sonnet-4-6"))
	deps.Registry = fr
	p := withFakePusher(t, &deps)
	queueDecision(p, true, bytesAllB(32, 0xCC))

	rep := newStubReputation()
	deps.Reputation = rep
	// Freeze a different identity than the envelope's consumer.
	rep.frozen[ids.IdentityID{0xAA}] = true

	b, err := OpenBroker(defaultBrokerCfg(), testSettlementCfg(), deps, nil)
	require.NoError(t, err)
	defer b.Close()

	env := makeAdmittedEnvelope(t, "claude-sonnet-4-6", 100, 200, 1_000_000)
	res, err := b.Submit(context.Background(), env)
	require.NoError(t, err)
	require.Equal(t, OutcomeAdmit, res.Outcome)
}

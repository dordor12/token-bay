package broker

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
)

// stubRevocationArchive lets tests control which identities are revoked.
type stubRevocationArchive struct {
	mu      sync.Mutex
	revoked map[ids.IdentityID]bool
	err     error
	calls   []ids.IdentityID
}

func newStubRevocationArchive() *stubRevocationArchive {
	return &stubRevocationArchive{revoked: map[ids.IdentityID]bool{}}
}

func (s *stubRevocationArchive) revoke(id ids.IdentityID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revoked[id] = true
}

func (s *stubRevocationArchive) IsIdentityRevoked(_ context.Context, id []byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var idArr ids.IdentityID
	copy(idArr[:], id)
	s.calls = append(s.calls, idArr)
	if s.err != nil {
		return false, s.err
	}
	return s.revoked[idArr], nil
}

func (s *stubRevocationArchive) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func TestSubmit_RejectsRevokedIdentity(t *testing.T) {
	deps := testDeps(t)
	fr := newFakeRegistry()
	fr.Add(seederRecord(t, ids.IdentityID{1}, 0.9, "claude-sonnet-4-6"))
	deps.Registry = fr
	p := withFakePusher(t, &deps)
	// Queue an offer decision so the test would otherwise admit; the
	// pre-check must short-circuit before any offer is dispatched.
	queueDecision(p, true, bytesAllB(32, 0xCC))

	arch := newStubRevocationArchive()
	deps.RevocationArchive = arch

	b, err := OpenBroker(defaultBrokerCfg(), testSettlementCfg(), deps, nil)
	require.NoError(t, err)
	defer b.Close()

	env := makeAdmittedEnvelope(t, "claude-sonnet-4-6", 100, 200, 1_000_000)
	var consumer ids.IdentityID
	copy(consumer[:], env.Body.ConsumerId)
	arch.revoke(consumer)

	_, err = b.Submit(context.Background(), env)
	require.ErrorIs(t, err, ErrIdentityFrozen)

	// The check must run before any reservation is held.
	require.Equal(t, uint64(0), b.mgr.Reservations.Reserved(consumer))
	require.GreaterOrEqual(t, arch.callCount(), 1)
}

func TestSubmit_AllowsCleanIdentity(t *testing.T) {
	deps := testDeps(t)
	fr := newFakeRegistry()
	fr.Add(seederRecord(t, ids.IdentityID{1}, 0.9, "claude-sonnet-4-6"))
	deps.Registry = fr
	p := withFakePusher(t, &deps)
	queueDecision(p, true, bytesAllB(32, 0xCC))

	arch := newStubRevocationArchive() // empty — no identities revoked
	deps.RevocationArchive = arch

	b, err := OpenBroker(defaultBrokerCfg(), testSettlementCfg(), deps, nil)
	require.NoError(t, err)
	defer b.Close()

	env := makeAdmittedEnvelope(t, "claude-sonnet-4-6", 100, 200, 1_000_000)
	res, err := b.Submit(context.Background(), env)
	require.NoError(t, err)
	require.Equal(t, OutcomeAdmit, res.Outcome)
	require.GreaterOrEqual(t, arch.callCount(), 1)
}

func TestSubmit_NilRevocationArchive_AllowsAll(t *testing.T) {
	// When the revocation archive is not wired (nil), Submit must
	// behave exactly as before — the v0 trackers had no cross-region
	// gossip and broker.Submit accepted everyone reputation-permitted.
	deps := testDeps(t)
	fr := newFakeRegistry()
	fr.Add(seederRecord(t, ids.IdentityID{1}, 0.9, "claude-sonnet-4-6"))
	deps.Registry = fr
	p := withFakePusher(t, &deps)
	queueDecision(p, true, bytesAllB(32, 0xCC))
	deps.RevocationArchive = nil

	b, err := OpenBroker(defaultBrokerCfg(), testSettlementCfg(), deps, nil)
	require.NoError(t, err)
	defer b.Close()

	env := makeAdmittedEnvelope(t, "claude-sonnet-4-6", 100, 200, 1_000_000)
	res, err := b.Submit(context.Background(), env)
	require.NoError(t, err)
	require.Equal(t, OutcomeAdmit, res.Outcome)
}

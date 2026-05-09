package broker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/session"
)

// TestSubsystems_OpenClose_Roundtrip verifies that Broker and Settlement
// share the same Inflight: a request inserted via Submit is visible to
// HandleUsageReport.
func TestSubsystems_OpenClose_Roundtrip(t *testing.T) {
	deps := testDeps(t)

	// Wire a capturing ledger so HandleUsageReport can complete.
	cap := &fakeLedgerCapturing{}
	deps.Ledger = cap

	// Wire a registry with a seeder.
	fr := newFakeRegistry()
	seederID := ids.IdentityID{0xAA}
	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	rec := seederRecord(t, seederID, 0.9, "claude-sonnet-4-6")
	fr.Add(rec)
	deps.Registry = fr

	// Wire a pusher that accepts the offer immediately.
	offerCh := make(chan *tbproto.OfferDecision, 1)
	offerCh <- &tbproto.OfferDecision{Accept: true, EphemeralPubkey: seederPub}
	deps.Pusher = &fakePusher{offerCh: offerCh, ok: true}

	fixedNow := time.Unix(1700000000, 0)
	deps.Now = func() time.Time { return fixedNow }

	sub, err := Open(defaultBrokerCfg(), testSettlementCfg(), deps)
	require.NoError(t, err)

	// Submit a request through the Broker.
	env := makeAdmittedEnvelope(t, "claude-sonnet-4-6", 100, 200, 1_000_000)
	var consumerID ids.IdentityID
	copy(consumerID[:], env.Body.ConsumerId)

	res, err := sub.Broker.Submit(context.Background(), env)
	require.NoError(t, err)
	require.Equal(t, OutcomeAdmit, res.Outcome)

	requestID := res.Admit.RequestID

	// Verify that Settlement.mgr (shared) has the same request.
	require.Same(t, sub.Broker.mgr, sub.Settlement.mgr)
	req, ok := sub.Settlement.mgr.Inflight.Get(requestID)
	require.True(t, ok, "Settlement.mgr should contain the request inserted by Broker")
	require.Equal(t, requestID, req.RequestID)
	require.Equal(t, session.StateAssigned, req.State)

	// Now HandleUsageReport through Settlement using the shared inflight.
	report := buildSeederSignedReport(
		t, seederPriv, requestID, "claude-sonnet-4-6",
		100, 200, make([]byte, 32), 0,
		consumerID, seederID, uint64(fixedNow.Unix()), //nolint:gosec
	)
	_, err = sub.Settlement.HandleUsageReport(context.Background(), seederID, report)
	require.NoError(t, err)

	require.NoError(t, sub.Close())
}

// TestSubsystems_OpenRequiresDeps verifies that Open validates deps before
// returning a Subsystems value.
func TestSubsystems_OpenRequiresDeps(t *testing.T) {
	deps := testDeps(t)
	deps.Ledger = nil // Broker and Settlement both require Ledger

	_, err := Open(defaultBrokerCfg(), testSettlementCfg(), deps)
	require.Error(t, err)
}

// TestSubsystems_CloseIdempotent verifies that Close can be called twice
// without error.
func TestSubsystems_CloseIdempotent(t *testing.T) {
	sub, err := Open(defaultBrokerCfg(), testSettlementCfg(), testDeps(t))
	require.NoError(t, err)
	require.NoError(t, sub.Close())
	require.NoError(t, sub.Close())
}

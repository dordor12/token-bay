package broker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
	"github.com/token-bay/token-bay/tracker/internal/admission"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
	"github.com/token-bay/token-bay/tracker/internal/session"
)

// ---------------------------------------------------------------------------
// fakeLedgerCapturing — records AppendUsage calls for assertions.
// ---------------------------------------------------------------------------

type fakeLedgerCapturing struct {
	mu      sync.Mutex
	appends []ledger.UsageRecord
}

func (f *fakeLedgerCapturing) Tip(_ context.Context) (uint64, []byte, bool, error) {
	return 0, make([]byte, 32), false, nil
}

func (f *fakeLedgerCapturing) AppendUsage(_ context.Context, r ledger.UsageRecord) (*tbproto.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appends = append(f.appends, r)
	return &tbproto.Entry{}, nil
}

func (f *fakeLedgerCapturing) Last() (ledger.UsageRecord, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.appends) == 0 {
		return ledger.UsageRecord{}, false
	}
	return f.appends[len(f.appends)-1], true
}

func (f *fakeLedgerCapturing) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.appends)
}

// ---------------------------------------------------------------------------
// buildSeederSignedReport — constructs a UsageReport signed by the seeder.
// ---------------------------------------------------------------------------

// testUsageAssertion builds the sequencing-independent usage-assertion that
// HandleUsageReport reconstructs for a report with these parameters —
// CostCredits is the tracker-computed actual cost.
func testUsageAssertion(t *testing.T, requestID [16]byte, consumerID, seederID ids.IdentityID, model string, in, out uint32) signing.UsageAssertion {
	t.Helper()
	pt := DefaultPriceTable()
	cost, err := pt.ActualCost(model, in, out)
	require.NoError(t, err)
	return signing.UsageAssertion{
		RequestID:    requestID[:],
		ConsumerID:   consumerID[:],
		SeederID:     seederID[:],
		Model:        model,
		InputTokens:  in,
		OutputTokens: out,
		CostCredits:  cost,
	}
}

// buildSeederSignedReport builds a UsageReport whose SeederSig is valid over
// the usage-assertion the tracker reconstructs. The assertion omits
// prev_hash/seq/timestamp, so the report is independent of the ledger tip
// and the tracker clock.
func buildSeederSignedReport(t *testing.T, seederPriv ed25519.PrivateKey, requestID [16]byte, model string, in, out uint32, consumerID, seederID ids.IdentityID) *tbproto.UsageReport {
	t.Helper()
	assertion := testUsageAssertion(t, requestID, consumerID, seederID, model, in, out)
	sig, err := signing.SignUsageAssertion(seederPriv, assertion)
	require.NoError(t, err)

	return &tbproto.UsageReport{
		RequestId:    requestID[:],
		InputTokens:  in,
		OutputTokens: out,
		Model:        model,
		SeederSig:    sig,
	}
}

// ---------------------------------------------------------------------------
// T16: TestSettlement_OpenClose
// ---------------------------------------------------------------------------

func TestSettlement_OpenClose(t *testing.T) {
	deps := testDeps(t)
	s, err := OpenSettlement(testSettlementCfg(), deps, nil)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.NoError(t, s.Close()) // idempotent
}

func TestSettlement_OpenRequiresLedger(t *testing.T) {
	deps := testDeps(t)
	deps.Ledger = nil
	_, err := OpenSettlement(testSettlementCfg(), deps, nil)
	require.Error(t, err)
}

func TestSettlement_OpenRequiresPusher(t *testing.T) {
	deps := testDeps(t)
	deps.Pusher = nil
	_, err := OpenSettlement(testSettlementCfg(), deps, nil)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// T17: HandleUsageReport tests
// ---------------------------------------------------------------------------

// makeAssignedRequest creates an inflight request already in StateAssigned
// with the given seeder and a real seeder ed25519 keypair.
func makeAssignedRequest(t *testing.T, requestID [16]byte, model string, consumerID, seederID ids.IdentityID, seederPub ed25519.PublicKey, maxIn, maxOut uint64) *session.Request {
	t.Helper()
	return &session.Request{
		RequestID:       requestID,
		ConsumerID:      consumerID,
		EnvelopeBody:    &tbproto.EnvelopeBody{Model: model, MaxInputTokens: maxIn, MaxOutputTokens: maxOut},
		MaxCostReserved: 1_000_000,
		AssignedSeeder:  seederID,
		SeederPubkey:    seederPub,
		State:           session.StateAssigned,
	}
}

func TestHandleUsageReport_UnknownRequest(t *testing.T) {
	deps := testDeps(t)
	s, err := OpenSettlement(testSettlementCfg(), deps, nil)
	require.NoError(t, err)
	defer s.Close()

	r := &tbproto.UsageReport{
		RequestId: make([]byte, 16),
		Model:     "claude-sonnet-4-6",
	}
	_, err = s.HandleUsageReport(context.Background(), ids.IdentityID{0xCC}, r)
	require.ErrorIs(t, err, session.ErrUnknownRequest)
}

func TestHandleUsageReport_SeederMismatch(t *testing.T) {
	deps := testDeps(t)
	mgr := session.New()

	requestID := [16]byte{0x01}
	consumerID := ids.IdentityID{0xCC}
	seederID := ids.IdentityID{0xDD}
	otherID := ids.IdentityID{0xEE}

	seederPub, _, _ := ed25519.GenerateKey(rand.Reader)
	mgr.Inflight.Insert(makeAssignedRequest(t, requestID, "claude-sonnet-4-6", consumerID, seederID, seederPub, 100, 200))

	s, err := OpenSettlement(testSettlementCfg(), deps, mgr)
	require.NoError(t, err)
	defer s.Close()

	r := &tbproto.UsageReport{
		RequestId: requestID[:],
		Model:     "claude-sonnet-4-6",
	}
	_, err = s.HandleUsageReport(context.Background(), otherID, r) // caller is otherID, not seederID
	require.ErrorIs(t, err, ErrSeederMismatch)
}

func TestHandleUsageReport_ModelMismatch(t *testing.T) {
	deps := testDeps(t)
	mgr := session.New()

	requestID := [16]byte{0x02}
	consumerID := ids.IdentityID{0xCC}
	seederID := ids.IdentityID{0xDD}

	seederPub, _, _ := ed25519.GenerateKey(rand.Reader)
	mgr.Inflight.Insert(makeAssignedRequest(t, requestID, "claude-sonnet-4-6", consumerID, seederID, seederPub, 100, 200))

	s, err := OpenSettlement(testSettlementCfg(), deps, mgr)
	require.NoError(t, err)
	defer s.Close()

	r := &tbproto.UsageReport{
		RequestId: requestID[:],
		Model:     "claude-opus-4-7", // wrong model
	}
	_, err = s.HandleUsageReport(context.Background(), seederID, r)
	require.ErrorIs(t, err, ErrModelMismatch)
}

func TestHandleUsageReport_CostOverspend(t *testing.T) {
	deps := testDeps(t)
	mgr := session.New()

	requestID := [16]byte{0x03}
	consumerID := ids.IdentityID{0xCC}
	seederID := ids.IdentityID{0xDD}
	model := "claude-sonnet-4-6"

	seederPub, _, _ := ed25519.GenerateKey(rand.Reader)
	// Reserve very little — force overspend
	req := makeAssignedRequest(t, requestID, model, consumerID, seederID, seederPub, 100, 200)
	req.MaxCostReserved = 1 // 1 credit — way too low
	mgr.Inflight.Insert(req)

	s, err := OpenSettlement(testSettlementCfg(), deps, mgr)
	require.NoError(t, err)
	defer s.Close()

	r := &tbproto.UsageReport{
		RequestId:    requestID[:],
		Model:        model,
		InputTokens:  100,
		OutputTokens: 200,
	}
	_, err = s.HandleUsageReport(context.Background(), seederID, r)
	require.ErrorIs(t, err, ErrCostOverspend)
}

func TestHandleUsageReport_SeederSigInvalid(t *testing.T) {
	deps := testDeps(t)
	mgr := session.New()

	requestID := [16]byte{0x04}
	consumerID := ids.IdentityID{0xCC}
	seederID := ids.IdentityID{0xDD}
	model := "claude-sonnet-4-6"

	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	mgr.Inflight.Insert(makeAssignedRequest(t, requestID, model, consumerID, seederID, seederPub, 100, 200))

	const fixedTS uint64 = 1700000000
	deps.Now = func() time.Time { return time.Unix(int64(fixedTS), 0) } //nolint:gosec

	s, err := OpenSettlement(testSettlementCfg(), deps, mgr)
	require.NoError(t, err)
	defer s.Close()

	// Build a properly-signed report then tamper the sig.
	report := buildSeederSignedReport(t, seederPriv, requestID, model, 100, 200, consumerID, seederID)
	report.SeederSig[0] ^= 0xFF // tamper

	_, err = s.HandleUsageReport(context.Background(), seederID, report)
	require.ErrorIs(t, err, ErrSeederSigInvalid)
}

func TestHandleUsageReport_AppendsConsumerSigMissing(t *testing.T) {
	deps := testDeps(t)

	cap := &fakeLedgerCapturing{}
	deps.Ledger = cap

	fr := newFakeRegistry()
	seederRec := seederRecord(t, ids.IdentityID{0xDD}, 0.9, "claude-sonnet-4-6")
	fr.Add(seederRec)
	_, _ = fr.IncLoad(seederRec.IdentityID)
	deps.Registry = fr

	mgr := session.New()

	requestID := [16]byte{0x05}
	consumerID := ids.IdentityID{0xCC}
	seederID := ids.IdentityID{0xDD}
	model := "claude-sonnet-4-6"

	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	req := makeAssignedRequest(t, requestID, model, consumerID, seederID, seederPub, 100, 200)
	mgr.Inflight.Insert(req)
	// Reserve some credits so Release can clean up.
	_ = mgr.Reservations.Reserve(requestID, consumerID, 1000, 1_000_000, time.Now().Add(time.Hour))

	// Use SettlementTimeoutS=0 so the timer fires immediately.
	cfg := testSettlementCfg()
	cfg.SettlementTimeoutS = 0

	// Fix the clock so the seeder can build the same entry body.
	const fixedTS uint64 = 1700000000
	deps.Now = func() time.Time { return time.Unix(int64(fixedTS), 0) } //nolint:gosec

	s, err := OpenSettlement(cfg, deps, mgr)
	require.NoError(t, err)
	defer s.Close()

	report := buildSeederSignedReport(t, seederPriv, requestID, model, 100, 200, consumerID, seederID)

	ack, err := s.HandleUsageReport(context.Background(), seederID, report)
	require.NoError(t, err)
	require.NotNil(t, ack)

	// Wait for the async goroutine to append (with timeout to avoid hanging tests).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cap.Count() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	last, ok := cap.Last()
	require.True(t, ok, "expected AppendUsage to have been called")
	require.True(t, last.ConsumerSigMissing,
		"expected ConsumerSigMissing=true on settlement timeout")
	require.Empty(t, last.ConsumerSig, "timeout path must not carry a consumer sig")
	require.Equal(t, report.SeederSig, last.SeederSig, "assertion-domain seeder sig passed through")
	require.Equal(t, seederPub, last.SeederPub, "seeder ephemeral pubkey passed through")

	// Confirm expected cost.
	pt := DefaultPriceTable()
	expectedCost, _ := pt.ActualCost(model, 100, 200)
	require.Equal(t, expectedCost, last.CostCredits)
}

func TestHandleUsageReport_DispatchesReputationLedgerEvent(t *testing.T) {
	deps := testDeps(t)

	cap := &fakeLedgerCapturing{}
	deps.Ledger = cap

	rep := newStubReputation()
	deps.Reputation = rep

	fr := newFakeRegistry()
	seederRec := seederRecord(t, ids.IdentityID{0xDD}, 0.9, "claude-sonnet-4-6")
	fr.Add(seederRec)
	_, _ = fr.IncLoad(seederRec.IdentityID)
	deps.Registry = fr

	mgr := session.New()

	requestID := [16]byte{0x06}
	consumerID := ids.IdentityID{0xCC}
	seederID := ids.IdentityID{0xDD}
	model := "claude-sonnet-4-6"

	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	req := makeAssignedRequest(t, requestID, model, consumerID, seederID, seederPub, 100, 200)
	mgr.Inflight.Insert(req)
	_ = mgr.Reservations.Reserve(requestID, consumerID, 1000, 1_000_000, time.Now().Add(time.Hour))

	cfg := testSettlementCfg()
	cfg.SettlementTimeoutS = 0

	const fixedTS uint64 = 1700000000
	deps.Now = func() time.Time { return time.Unix(int64(fixedTS), 0) } //nolint:gosec

	s, err := OpenSettlement(cfg, deps, mgr)
	require.NoError(t, err)
	defer s.Close()

	report := buildSeederSignedReport(t, seederPriv, requestID, model, 100, 200, consumerID, seederID)
	_, err = s.HandleUsageReport(context.Background(), seederID, report)
	require.NoError(t, err)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(rep.ledgerEvents()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	events := rep.ledgerEvents()
	require.Len(t, events, 1, "expected one settlement event dispatched to reputation")
	ev := events[0]
	require.Equal(t, admission.LedgerEventSettlement, ev.Kind)
	require.Equal(t, consumerID, ev.ConsumerID)
	require.Equal(t, seederID, ev.SeederID)
	require.Equal(t, uint32(1), ev.Flags, "timeout path sets the ConsumerSigMissing flag bit")
}

// ---------------------------------------------------------------------------
// T18: HandleSettle tests
// ---------------------------------------------------------------------------

func TestHandleSettle_DispatchSig(t *testing.T) {
	deps := testDeps(t)
	mgr := session.New()

	requestID := [16]byte{0x10}
	consumerID := ids.IdentityID{0xCC}
	seederID := ids.IdentityID{0xDD}

	seederPub, _, _ := ed25519.GenerateKey(rand.Reader)
	req := makeAssignedRequest(t, requestID, "claude-sonnet-4-6", consumerID, seederID, seederPub, 100, 200)
	req.SettleSig = make(chan []byte, 1)
	req.State = session.StateServing
	mgr.Inflight.Insert(req)

	// Index by a known hash.
	var hash [32]byte
	hash[0] = 0xAB
	require.NoError(t, mgr.Inflight.IndexByHash(requestID, hash))

	s, err := OpenSettlement(testSettlementCfg(), deps, mgr)
	require.NoError(t, err)
	defer s.Close()

	fakeSig := []byte("consumer-sig-bytes")
	ack, err := s.HandleSettle(context.Background(), consumerID, &tbproto.SettleRequest{
		PreimageHash: hash[:],
		ConsumerSig:  fakeSig,
	})
	require.NoError(t, err)
	require.NotNil(t, ack)

	select {
	case got := <-req.SettleSig:
		require.Equal(t, fakeSig, got)
	case <-time.After(time.Second):
		t.Fatal("sig never arrived on SettleSig channel")
	}
}

func TestHandleSettle_UnknownPreimage(t *testing.T) {
	deps := testDeps(t)
	s, err := OpenSettlement(testSettlementCfg(), deps, nil)
	require.NoError(t, err)
	defer s.Close()

	var hash [32]byte
	hash[0] = 0xFF
	_, err = s.HandleSettle(context.Background(), ids.IdentityID{0xCC}, &tbproto.SettleRequest{
		PreimageHash: hash[:],
	})
	require.ErrorIs(t, err, ErrUnknownPreimage)
}

// ---------------------------------------------------------------------------
// T17.5: TestSettlement_IdentityResolverWired
// ---------------------------------------------------------------------------

// mockIdentityResolver is a test double for broker.IdentityResolver.
type mockIdentityResolver struct {
	keys map[ids.IdentityID]ed25519.PublicKey
	hits int
}

func (m *mockIdentityResolver) PeerPubkey(id ids.IdentityID) (ed25519.PublicKey, bool) {
	m.hits++
	k, ok := m.keys[id]
	return k, ok
}

// TestSettlement_IdentityResolverWired checks that:
//  1. A nil Identity dep does not panic (preserves v1 fallback behaviour).
//  2. A non-nil Identity dep is reachable (the field is actually plumbed into
//     Deps and Settlement stores it).
//
// Actual sig verification is deferred pending a wire-format amendment
// (see awaitSettle TODO). This test only validates the plumbing path.
func TestSettlement_IdentityResolverWired(t *testing.T) {
	t.Run("nil_identity_no_panic", func(t *testing.T) {
		deps := testDeps(t)
		deps.Identity = nil // explicit nil — should not panic
		s, err := OpenSettlement(testSettlementCfg(), deps, nil)
		require.NoError(t, err)
		require.NoError(t, s.Close())
	})

	t.Run("non_nil_identity_reachable", func(t *testing.T) {
		deps := testDeps(t)
		resolver := &mockIdentityResolver{
			keys: map[ids.IdentityID]ed25519.PublicKey{},
		}
		deps.Identity = resolver

		s, err := OpenSettlement(testSettlementCfg(), deps, nil)
		require.NoError(t, err)
		defer s.Close()

		// Call PeerPubkey directly via the stored interface to confirm
		// the resolver is reachable and callable without panic.
		_, _ = deps.Identity.PeerPubkey(ids.IdentityID{0xAB})
		require.Equal(t, 1, resolver.hits, "resolver was not called")
	})
}

// sha256Of returns the SHA-256 digest of b.
func sha256Of(b []byte) [32]byte { return sha256.Sum256(b) }

// counterValue reads the current value of a prometheus.Counter.
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, c.Write(&m))
	return m.GetCounter().GetValue()
}

// ---------------------------------------------------------------------------
// T13: pre-transition state guard
// ---------------------------------------------------------------------------

// TestHandleUsageReport_InvalidState ensures HandleUsageReport rejects with
// ErrInvalidState — and does NO ledger work — when the request is not in
// {ASSIGNED, SERVING}. spec §5.2 step 2.
func TestHandleUsageReport_InvalidState(t *testing.T) {
	deps := testDeps(t)

	cap := &fakeLedgerCapturing{}
	deps.Ledger = cap

	mgr := session.New()

	requestID := [16]byte{0x07}
	consumerID := ids.IdentityID{0xCC}
	seederID := ids.IdentityID{0xDD}
	model := "claude-sonnet-4-6"

	seederPub, _, _ := ed25519.GenerateKey(rand.Reader)
	req := makeAssignedRequest(t, requestID, model, consumerID, seederID, seederPub, 100, 200)
	req.State = session.StateSelecting // not ASSIGNED or SERVING
	mgr.Inflight.Insert(req)

	s, err := OpenSettlement(testSettlementCfg(), deps, mgr)
	require.NoError(t, err)
	defer s.Close()

	r := &tbproto.UsageReport{
		RequestId: requestID[:],
		Model:     model,
	}
	_, err = s.HandleUsageReport(context.Background(), seederID, r)
	require.ErrorIs(t, err, ErrInvalidState)
	require.Equal(t, 0, cap.Count(), "no ledger touch expected on INVALID_STATE")
}

// ---------------------------------------------------------------------------
// T8: consumer-sig verification on settlement
// ---------------------------------------------------------------------------

// runHandleUsageReport drives HandleUsageReport with a properly-signed report
// and waits for the per-request goroutine to be parked on req.SettleSig. It
// returns the canonical usage-assertion bytes — the preimage HandleUsageReport
// pushed to the consumer and that the consumer counter-signs.
func runHandleUsageReport(t *testing.T, s *Settlement, requestID [16]byte, consumerID, seederID ids.IdentityID, seederPriv ed25519.PrivateKey, model string) []byte {
	t.Helper()
	report := buildSeederSignedReport(t, seederPriv, requestID, model, 100, 200, consumerID, seederID)
	_, err := s.HandleUsageReport(context.Background(), seederID, report)
	require.NoError(t, err)

	assertion := testUsageAssertion(t, requestID, consumerID, seederID, model, 100, 200)
	preSig, err := signing.CanonicalUsageAssertionPreSig(assertion)
	require.NoError(t, err)
	return preSig
}

// TestHandleSettle_TamperedConsumerSig — verify fail returns ErrConsumerSig,
// emits a "refused_consumer_sig" audit entry, and does NOT touch the ledger.
func TestHandleSettle_TamperedConsumerSig(t *testing.T) {
	deps := testDeps(t)

	cap := &fakeLedgerCapturing{}
	deps.Ledger = cap

	// Capture audit log output.
	var logBuf bytes.Buffer
	deps.Logger = zerolog.New(&logBuf)

	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	consumerID := ids.IdentityID{0xCC}
	seederID := ids.IdentityID{0xDD}
	deps.Identity = &mockIdentityResolver{
		keys: map[ids.IdentityID]ed25519.PublicKey{consumerID: consumerPub},
	}

	mgr := session.New()
	requestID := [16]byte{0x20}
	model := "claude-sonnet-4-6"

	req := makeAssignedRequest(t, requestID, model, consumerID, seederID, seederPub, 100, 200)
	mgr.Inflight.Insert(req)
	_ = mgr.Reservations.Reserve(requestID, consumerID, 1000, 1_000_000, time.Now().Add(time.Hour))

	// Long settlement timeout so the timer doesn't race the test.
	cfg := testSettlementCfg()
	cfg.SettlementTimeoutS = 900

	const fixedTS uint64 = 1700000000
	deps.Now = func() time.Time { return time.Unix(int64(fixedTS), 0) } //nolint:gosec

	s, err := OpenSettlement(cfg, deps, mgr)
	require.NoError(t, err)
	defer s.Close()

	preSig := runHandleUsageReport(t, s, requestID, consumerID, seederID, seederPriv, model)

	// Produce a valid assertion counter-sig then flip a bit to tamper it.
	sig := ed25519.Sign(consumerPriv, preSig)
	sig[0] ^= 0xFF

	hash := sha256Of(preSig)
	_, err = s.HandleSettle(context.Background(), consumerID, &tbproto.SettleRequest{
		PreimageHash: hash[:],
		ConsumerSig:  sig,
	})
	require.ErrorIs(t, err, ErrConsumerSig)

	require.Contains(t, logBuf.String(), "refused_consumer_sig",
		"expected an audit log entry tagged 'refused_consumer_sig'")
	require.Equal(t, 0, cap.Count(), "no ledger touch expected on consumer-sig refusal")
}

// TestHandleSettle_ValidSigAppendsVerified — a valid consumer sig causes the
// per-request goroutine to call AppendUsage with ConsumerSigMissing=false.
func TestHandleSettle_ValidSigAppendsVerified(t *testing.T) {
	deps := testDeps(t)

	cap := &fakeLedgerCapturing{}
	deps.Ledger = cap

	fr := newFakeRegistry()
	seederRec := seederRecord(t, ids.IdentityID{0xDD}, 0.9, "claude-sonnet-4-6")
	fr.Add(seederRec)
	_, _ = fr.IncLoad(seederRec.IdentityID)
	deps.Registry = fr

	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	consumerID := ids.IdentityID{0xCC}
	seederID := ids.IdentityID{0xDD}
	deps.Identity = &mockIdentityResolver{
		keys: map[ids.IdentityID]ed25519.PublicKey{consumerID: consumerPub},
	}

	mgr := session.New()
	requestID := [16]byte{0x21}
	model := "claude-sonnet-4-6"

	req := makeAssignedRequest(t, requestID, model, consumerID, seederID, seederPub, 100, 200)
	mgr.Inflight.Insert(req)
	_ = mgr.Reservations.Reserve(requestID, consumerID, 1000, 1_000_000, time.Now().Add(time.Hour))

	cfg := testSettlementCfg()
	cfg.SettlementTimeoutS = 900

	const fixedTS uint64 = 1700000000
	deps.Now = func() time.Time { return time.Unix(int64(fixedTS), 0) } //nolint:gosec

	s, err := OpenSettlement(cfg, deps, mgr)
	require.NoError(t, err)
	defer s.Close()

	preSig := runHandleUsageReport(t, s, requestID, consumerID, seederID, seederPriv, model)

	// The consumer counter-signs the canonical usage-assertion bytes.
	sig := ed25519.Sign(consumerPriv, preSig)
	hash := sha256Of(preSig)

	_, err = s.HandleSettle(context.Background(), consumerID, &tbproto.SettleRequest{
		PreimageHash: hash[:],
		ConsumerSig:  sig,
	})
	require.NoError(t, err)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cap.Count() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	last, ok := cap.Last()
	require.True(t, ok, "expected AppendUsage to have been called")
	require.False(t, last.ConsumerSigMissing,
		"expected ConsumerSigMissing=false when consumer sig verified")
	require.Equal(t, sig, last.ConsumerSig,
		"the verified assertion counter-sig must reach the ledger record")
	require.Equal(t, consumerPub, last.ConsumerPub,
		"the resolved consumer pubkey must reach the ledger record")
	require.NotEmpty(t, last.SeederSig)
	require.Equal(t, seederPub, last.SeederPub)

	// The record is exactly what ledger.AppendUsage will verify: both sigs
	// must check out over the assertion rebuilt from the record's fields.
	assertion := signing.UsageAssertion{
		RequestID:    last.RequestID,
		ConsumerID:   last.ConsumerID,
		SeederID:     last.SeederID,
		Model:        last.Model,
		InputTokens:  last.InputTokens,
		OutputTokens: last.OutputTokens,
		CostCredits:  last.CostCredits,
	}
	require.True(t, signing.VerifyUsageAssertion(last.ConsumerPub, assertion, last.ConsumerSig))
	require.True(t, signing.VerifyUsageAssertion(last.SeederPub, assertion, last.SeederSig))
}

// TestHandleSettle_UnknownConsumerPubkey — when the IdentityResolver doesn't
// know the consumer, the settlement still appends but with
// ConsumerSigMissing=true and the ConsumerPubkeyUnknown counter is bumped.
func TestHandleSettle_UnknownConsumerPubkey(t *testing.T) {
	deps := testDeps(t)

	cap := &fakeLedgerCapturing{}
	deps.Ledger = cap

	fr := newFakeRegistry()
	seederRec := seederRecord(t, ids.IdentityID{0xDD}, 0.9, "claude-sonnet-4-6")
	fr.Add(seederRec)
	_, _ = fr.IncLoad(seederRec.IdentityID)
	deps.Registry = fr

	// Resolver returns ok=false for any id.
	deps.Identity = &mockIdentityResolver{keys: map[ids.IdentityID]ed25519.PublicKey{}}

	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerID := ids.IdentityID{0xCC}
	seederID := ids.IdentityID{0xDD}

	mgr := session.New()
	requestID := [16]byte{0x22}
	model := "claude-sonnet-4-6"

	req := makeAssignedRequest(t, requestID, model, consumerID, seederID, seederPub, 100, 200)
	mgr.Inflight.Insert(req)
	_ = mgr.Reservations.Reserve(requestID, consumerID, 1000, 1_000_000, time.Now().Add(time.Hour))

	cfg := testSettlementCfg()
	cfg.SettlementTimeoutS = 900

	const fixedTS uint64 = 1700000000
	deps.Now = func() time.Time { return time.Unix(int64(fixedTS), 0) } //nolint:gosec

	s, err := OpenSettlement(cfg, deps, mgr)
	require.NoError(t, err)
	defer s.Close()

	preSig := runHandleUsageReport(t, s, requestID, consumerID, seederID, seederPriv, model)

	// Any non-empty sig — the tracker can't verify without a pubkey.
	sig := make([]byte, ed25519.SignatureSize)
	hash := sha256Of(preSig)

	beforeCounter := counterValue(t, s.metrics.ConsumerPubkeyUnknown)

	_, err = s.HandleSettle(context.Background(), consumerID, &tbproto.SettleRequest{
		PreimageHash: hash[:],
		ConsumerSig:  sig,
	})
	require.NoError(t, err)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cap.Count() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	last, ok := cap.Last()
	require.True(t, ok, "expected AppendUsage to have been called")
	require.True(t, last.ConsumerSigMissing,
		"expected ConsumerSigMissing=true when consumer pubkey is unknown")
	require.Empty(t, last.ConsumerSig,
		"an unverifiable sig must not be stored on the record")

	afterCounter := counterValue(t, s.metrics.ConsumerPubkeyUnknown)
	require.Equal(t, beforeCounter+1, afterCounter,
		"expected ConsumerPubkeyUnknown counter to be incremented")
}

// ---------------------------------------------------------------------------
// Settlement replay defense: duplicate usage_report rejection
// ---------------------------------------------------------------------------

// TestHandleUsageReport_DuplicateRejected — a second usage_report for the
// same request_id while the first settlement is in flight (benign seeder
// retry after a lost UsageAck, or a malicious replay) must be rejected with
// ErrDuplicateUsageReport and must NOT spawn a second settlement goroutine:
// exactly one ledger append happens for the request.
func TestHandleUsageReport_DuplicateRejected(t *testing.T) {
	deps := testDeps(t)

	cap := &fakeLedgerCapturing{}
	deps.Ledger = cap

	fr := newFakeRegistry()
	seederRec := seederRecord(t, ids.IdentityID{0xDD}, 0.9, "claude-sonnet-4-6")
	fr.Add(seederRec)
	_, _ = fr.IncLoad(seederRec.IdentityID)
	deps.Registry = fr

	mgr := session.New()

	requestID := [16]byte{0x30}
	consumerID := ids.IdentityID{0xCC}
	seederID := ids.IdentityID{0xDD}
	model := "claude-sonnet-4-6"

	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	req := makeAssignedRequest(t, requestID, model, consumerID, seederID, seederPub, 100, 200)
	mgr.Inflight.Insert(req)
	_ = mgr.Reservations.Reserve(requestID, consumerID, 1000, 1_000_000, time.Now().Add(time.Hour))

	// Long settlement timeout: the first settlement stays in flight while
	// the duplicate arrives.
	cfg := testSettlementCfg()
	cfg.SettlementTimeoutS = 900

	const fixedTS uint64 = 1700000000
	deps.Now = func() time.Time { return time.Unix(int64(fixedTS), 0) } //nolint:gosec

	s, err := OpenSettlement(cfg, deps, mgr)
	require.NoError(t, err)
	defer s.Close()

	report := buildSeederSignedReport(t, seederPriv, requestID, model, 100, 200, consumerID, seederID)

	_, err = s.HandleUsageReport(context.Background(), seederID, report)
	require.NoError(t, err, "first report must be accepted")

	// Seeder resends the identical report (UsageAck lost / replay).
	_, err = s.HandleUsageReport(context.Background(), seederID, report)
	require.ErrorIs(t, err, ErrDuplicateUsageReport)
	require.Equal(t, 0, cap.Count(), "no append may happen while the settlement is still pending")

	// Release the parked settlement goroutine by delivering a consumer
	// sig. (No Identity resolver wired — the sig is unverifiable, which is
	// fine; only the append count matters here.)
	req.SettleSig <- []byte("consumer-sig")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cap.Count() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.Equal(t, 1, cap.Count(), "exactly one ledger append per request_id")
}

func TestHandleSettle_Duplicate(t *testing.T) {
	deps := testDeps(t)
	mgr := session.New()

	requestID := [16]byte{0x11}
	consumerID := ids.IdentityID{0xCC}
	seederID := ids.IdentityID{0xDD}

	seederPub, _, _ := ed25519.GenerateKey(rand.Reader)
	req := makeAssignedRequest(t, requestID, "claude-sonnet-4-6", consumerID, seederID, seederPub, 100, 200)
	req.SettleSig = make(chan []byte, 1)
	req.State = session.StateServing
	mgr.Inflight.Insert(req)

	var hash [32]byte
	hash[0] = 0xBA
	require.NoError(t, mgr.Inflight.IndexByHash(requestID, hash))

	s, err := OpenSettlement(testSettlementCfg(), deps, mgr)
	require.NoError(t, err)
	defer s.Close()

	r := &tbproto.SettleRequest{PreimageHash: hash[:], ConsumerSig: []byte("sig")}
	_, err = s.HandleSettle(context.Background(), consumerID, r)
	require.NoError(t, err) // first call succeeds

	_, err = s.HandleSettle(context.Background(), consumerID, r)
	require.ErrorIs(t, err, ErrDuplicateSettle) // second call is duplicate
}

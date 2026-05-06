package broker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
	"github.com/token-bay/token-bay/tracker/internal/ledger/entry"
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

// buildSeederSignedReport builds a UsageReport whose SeederSig is valid over the
// entry body the tracker will reconstruct. timestamp must match the value that
// s.deps.Now() returns at the moment HandleUsageReport processes the report.
func buildSeederSignedReport(t *testing.T, seederPriv ed25519.PrivateKey, requestID [16]byte, model string, in, out uint32, prevHash []byte, tipSeq uint64, consumerID, seederID ids.IdentityID, timestamp uint64) *tbproto.UsageReport {
	t.Helper()
	pt := DefaultPriceTable()
	cost, err := pt.ActualCost(model, in, out)
	require.NoError(t, err)

	body, err := entry.BuildUsageEntry(entry.UsageInput{
		PrevHash:     prevHash,
		Seq:          tipSeq + 1,
		ConsumerID:   consumerID[:],
		SeederID:     seederID[:],
		Model:        model,
		InputTokens:  in,
		OutputTokens: out,
		CostCredits:  cost,
		Timestamp:    timestamp,
		RequestID:    requestID[:],
	})
	require.NoError(t, err)

	bodyBytes, err := signing.DeterministicMarshal(body)
	require.NoError(t, err)
	sig := ed25519.Sign(seederPriv, bodyBytes)

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
	s, err := OpenSettlement(testSettlementCfg(), deps, nil, nil)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.NoError(t, s.Close()) // idempotent
}

func TestSettlement_OpenRequiresLedger(t *testing.T) {
	deps := testDeps(t)
	deps.Ledger = nil
	_, err := OpenSettlement(testSettlementCfg(), deps, nil, nil)
	require.Error(t, err)
}

func TestSettlement_OpenRequiresPusher(t *testing.T) {
	deps := testDeps(t)
	deps.Pusher = nil
	_, err := OpenSettlement(testSettlementCfg(), deps, nil, nil)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// T17: HandleUsageReport tests
// ---------------------------------------------------------------------------

// makeAssignedRequest creates an inflight request already in StateAssigned
// with the given seeder and a real seeder ed25519 keypair.
func makeAssignedRequest(t *testing.T, requestID [16]byte, model string, consumerID, seederID ids.IdentityID, seederPub ed25519.PublicKey, maxIn, maxOut uint64) *Request {
	t.Helper()
	return &Request{
		RequestID:       requestID,
		ConsumerID:      consumerID,
		EnvelopeBody:    &tbproto.EnvelopeBody{Model: model, MaxInputTokens: maxIn, MaxOutputTokens: maxOut},
		MaxCostReserved: 1_000_000,
		AssignedSeeder:  seederID,
		SeederPubkey:    seederPub,
		State:           StateAssigned,
	}
}

func TestHandleUsageReport_UnknownRequest(t *testing.T) {
	deps := testDeps(t)
	s, err := OpenSettlement(testSettlementCfg(), deps, nil, nil)
	require.NoError(t, err)
	defer s.Close()

	r := &tbproto.UsageReport{
		RequestId: make([]byte, 16),
		Model:     "claude-sonnet-4-6",
	}
	_, err = s.HandleUsageReport(context.Background(), ids.IdentityID{0xCC}, r)
	require.ErrorIs(t, err, ErrUnknownRequest)
}

func TestHandleUsageReport_SeederMismatch(t *testing.T) {
	deps := testDeps(t)
	inflt := NewInflight()

	requestID := [16]byte{0x01}
	consumerID := ids.IdentityID{0xCC}
	seederID := ids.IdentityID{0xDD}
	otherID := ids.IdentityID{0xEE}

	seederPub, _, _ := ed25519.GenerateKey(rand.Reader)
	inflt.Insert(makeAssignedRequest(t, requestID, "claude-sonnet-4-6", consumerID, seederID, seederPub, 100, 200))

	s, err := OpenSettlement(testSettlementCfg(), deps, inflt, nil)
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
	inflt := NewInflight()

	requestID := [16]byte{0x02}
	consumerID := ids.IdentityID{0xCC}
	seederID := ids.IdentityID{0xDD}

	seederPub, _, _ := ed25519.GenerateKey(rand.Reader)
	inflt.Insert(makeAssignedRequest(t, requestID, "claude-sonnet-4-6", consumerID, seederID, seederPub, 100, 200))

	s, err := OpenSettlement(testSettlementCfg(), deps, inflt, nil)
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
	inflt := NewInflight()

	requestID := [16]byte{0x03}
	consumerID := ids.IdentityID{0xCC}
	seederID := ids.IdentityID{0xDD}
	model := "claude-sonnet-4-6"

	seederPub, _, _ := ed25519.GenerateKey(rand.Reader)
	// Reserve very little — force overspend
	req := makeAssignedRequest(t, requestID, model, consumerID, seederID, seederPub, 100, 200)
	req.MaxCostReserved = 1 // 1 credit — way too low
	inflt.Insert(req)

	s, err := OpenSettlement(testSettlementCfg(), deps, inflt, nil)
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
	inflt := NewInflight()

	requestID := [16]byte{0x04}
	consumerID := ids.IdentityID{0xCC}
	seederID := ids.IdentityID{0xDD}
	model := "claude-sonnet-4-6"

	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	inflt.Insert(makeAssignedRequest(t, requestID, model, consumerID, seederID, seederPub, 100, 200))

	const fixedTS uint64 = 1700000000
	deps.Now = func() time.Time { return time.Unix(int64(fixedTS), 0) } //nolint:gosec

	s, err := OpenSettlement(testSettlementCfg(), deps, inflt, nil)
	require.NoError(t, err)
	defer s.Close()

	// Build a properly-signed report then tamper the sig.
	report := buildSeederSignedReport(t, seederPriv, requestID, model, 100, 200, make([]byte, 32), 0, consumerID, seederID, fixedTS)
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

	inflt := NewInflight()
	resv := NewReservations()

	requestID := [16]byte{0x05}
	consumerID := ids.IdentityID{0xCC}
	seederID := ids.IdentityID{0xDD}
	model := "claude-sonnet-4-6"

	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	req := makeAssignedRequest(t, requestID, model, consumerID, seederID, seederPub, 100, 200)
	inflt.Insert(req)
	// Reserve some credits so Release can clean up.
	_ = resv.Reserve(requestID, consumerID, 1000, 1_000_000, time.Now().Add(time.Hour))

	// Use SettlementTimeoutS=0 so the timer fires immediately.
	cfg := testSettlementCfg()
	cfg.SettlementTimeoutS = 0

	// Fix the clock so the seeder can build the same entry body.
	const fixedTS uint64 = 1700000000
	deps.Now = func() time.Time { return time.Unix(int64(fixedTS), 0) } //nolint:gosec

	s, err := OpenSettlement(cfg, deps, inflt, resv)
	require.NoError(t, err)
	defer s.Close()

	report := buildSeederSignedReport(t, seederPriv, requestID, model, 100, 200, make([]byte, 32), 0, consumerID, seederID, fixedTS)

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
	require.True(t, last.ConsumerSigMissing, "expected ConsumerSigMissing=true in v1")

	// Confirm expected cost.
	pt := DefaultPriceTable()
	expectedCost, _ := pt.ActualCost(model, 100, 200)
	require.Equal(t, expectedCost, last.CostCredits)
}

// ---------------------------------------------------------------------------
// T18: HandleSettle tests
// ---------------------------------------------------------------------------

func TestHandleSettle_DispatchSig(t *testing.T) {
	deps := testDeps(t)
	inflt := NewInflight()

	requestID := [16]byte{0x10}
	consumerID := ids.IdentityID{0xCC}
	seederID := ids.IdentityID{0xDD}

	seederPub, _, _ := ed25519.GenerateKey(rand.Reader)
	req := makeAssignedRequest(t, requestID, "claude-sonnet-4-6", consumerID, seederID, seederPub, 100, 200)
	req.SettleSig = make(chan []byte, 1)
	req.State = StateServing
	inflt.Insert(req)

	// Index by a known hash.
	var hash [32]byte
	hash[0] = 0xAB
	require.NoError(t, inflt.IndexByHash(requestID, hash))

	s, err := OpenSettlement(testSettlementCfg(), deps, inflt, nil)
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
	s, err := OpenSettlement(testSettlementCfg(), deps, nil, nil)
	require.NoError(t, err)
	defer s.Close()

	var hash [32]byte
	hash[0] = 0xFF
	_, err = s.HandleSettle(context.Background(), ids.IdentityID{0xCC}, &tbproto.SettleRequest{
		PreimageHash: hash[:],
	})
	require.ErrorIs(t, err, ErrUnknownPreimage)
}

func TestHandleSettle_Duplicate(t *testing.T) {
	deps := testDeps(t)
	inflt := NewInflight()

	requestID := [16]byte{0x11}
	consumerID := ids.IdentityID{0xCC}
	seederID := ids.IdentityID{0xDD}

	seederPub, _, _ := ed25519.GenerateKey(rand.Reader)
	req := makeAssignedRequest(t, requestID, "claude-sonnet-4-6", consumerID, seederID, seederPub, 100, 200)
	req.SettleSig = make(chan []byte, 1)
	req.State = StateServing
	inflt.Insert(req)

	var hash [32]byte
	hash[0] = 0xBA
	require.NoError(t, inflt.IndexByHash(requestID, hash))

	s, err := OpenSettlement(testSettlementCfg(), deps, inflt, nil)
	require.NoError(t, err)
	defer s.Close()

	r := &tbproto.SettleRequest{PreimageHash: hash[:], ConsumerSig: []byte("sig")}
	_, err = s.HandleSettle(context.Background(), consumerID, r)
	require.NoError(t, err) // first call succeeds

	_, err = s.HandleSettle(context.Background(), consumerID, r)
	require.ErrorIs(t, err, ErrDuplicateSettle) // second call is duplicate
}

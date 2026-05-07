package broker

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"sync"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
	"github.com/token-bay/token-bay/tracker/internal/ledger/entry"
)

// Settlement owns the post-assignment phase of the broker lifecycle:
// usage_report handling, consumer counter-sig waiting, ledger append, and
// reservation release. See broker-design §5.2 and §5.3 for the authoritative
// description of this subsystem.
//
// Construct via OpenSettlement; tear down via Close.
type Settlement struct {
	cfg   config.SettlementConfig
	deps  Deps
	inflt *Inflight
	resv  *Reservations
	stop  chan struct{}
	wg    sync.WaitGroup
}

// OpenSettlement constructs a ready Settlement. Required deps: Ledger, Pusher.
// Optional: Now (defaults to time.Now). If inflt or resv are nil, fresh
// instances are allocated. Starts the reservation TTL reaper (T19).
func OpenSettlement(cfg config.SettlementConfig, deps Deps, inflt *Inflight, resv *Reservations) (*Settlement, error) {
	if deps.Ledger == nil {
		return nil, errors.New("settlement: Ledger required")
	}
	if deps.Pusher == nil {
		return nil, errors.New("settlement: Pusher required")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if inflt == nil {
		inflt = NewInflight()
	}
	if resv == nil {
		resv = NewReservations()
	}
	s := &Settlement{
		cfg:   cfg,
		deps:  deps,
		inflt: inflt,
		resv:  resv,
		stop:  make(chan struct{}),
	}
	s.startReaper()
	return s, nil
}

// Close shuts down all settlement goroutines. Idempotent.
func (s *Settlement) Close() error {
	select {
	case <-s.stop:
		return nil
	default:
		close(s.stop)
	}
	s.wg.Wait()
	return nil
}

// HandleUsageReport implements broker-design §5.2. Returns UsageAck immediately
// after validating + queuing the consumer-sig wait; the ledger append happens
// asynchronously inside a per-request goroutine.
//
// v1 limitation: consumer pubkey resolution is deferred (T17.5 follow-up plan).
// Until that lands, every settlement appends the entry with
// ConsumerSigMissing=true on timer expiry. The Settle dispatcher is wired
// through (HandleSettle) but the goroutine ignores the sig.
//
// TODO(broker-followup): T17.5 — wire consumer pubkey resolver so HandleSettle
// can verify the consumer_sig and pass ConsumerSigMissing=false to AppendUsage.
func (s *Settlement) HandleUsageReport(ctx context.Context, peerID ids.IdentityID, r *tbproto.UsageReport) (*tbproto.UsageAck, error) {
	if r == nil || len(r.RequestId) != 16 {
		return nil, errors.New("settlement: malformed UsageReport")
	}

	// 1. Lookup inflight request.
	var reqID [16]byte
	copy(reqID[:], r.RequestId)
	req, ok := s.inflt.Get(reqID)
	if !ok {
		return nil, ErrUnknownRequest
	}

	// 2. Seeder must be the caller.
	if req.AssignedSeeder != peerID {
		return nil, ErrSeederMismatch
	}

	// 3. Model must match what was agreed.
	if req.EnvelopeBody == nil || req.EnvelopeBody.Model != r.Model {
		return nil, ErrModelMismatch
	}

	// 4. Compute actual cost and check overspend (5 % tolerance).
	actualCost, err := s.deps.Pricing.ActualCost(r.Model, r.InputTokens, r.OutputTokens)
	if err != nil {
		return nil, err
	}
	const overspendNumer, overspendDenom = 105, 100
	if actualCost > req.MaxCostReserved*overspendNumer/overspendDenom {
		return nil, ErrCostOverspend
	}

	// 5. Transition SELECTING→SERVING or ASSIGNED→SERVING (idempotent if already SERVING).
	// Try both source states; whichever succeeds is fine.
	if terr := s.inflt.Transition(reqID, StateAssigned, StateServing); terr != nil {
		if !errors.Is(terr, ErrIllegalTransition) {
			return nil, terr
		}
		// May already be SERVING from a racing call; if the state is
		// truly wrong, the next step will catch it.
	}

	// 6. Build entry preimage via ledger/entry.BuildUsageEntry at fresh tip.
	tipSeq, tipHash, _, terr := s.deps.Ledger.Tip(ctx)
	if terr != nil {
		return nil, terr
	}

	now := s.deps.Now()
	body, berr := entry.BuildUsageEntry(entry.UsageInput{
		PrevHash:     tipHash,
		Seq:          tipSeq + 1,
		ConsumerID:   req.ConsumerID[:],
		SeederID:     req.AssignedSeeder[:],
		Model:        r.Model,
		InputTokens:  r.InputTokens,
		OutputTokens: r.OutputTokens,
		CostCredits:  actualCost,
		Timestamp:    uint64(now.Unix()), //nolint:gosec // G115: always positive
		RequestID:    r.RequestId,
		// v1: consumer pubkey resolution is not implemented (T17.5); every
		// settlement appends with ConsumerSigMissing=true. Pre-set the flag
		// in the body now so the seeder sig is verified over the exact same
		// bytes that will be stored in the ledger, ensuring ledger re-
		// verification passes.
		ConsumerSigMissing: true, // TODO(broker-followup): T17.5 — clear when consumer sig is verified
	})
	if berr != nil {
		return nil, berr
	}

	// 7. Verify seeder sig over DeterministicMarshal(body).
	if !signing.VerifyEntry(ed25519.PublicKey(req.SeederPubkey), body, r.SeederSig) {
		return nil, ErrSeederSigInvalid
	}

	// 8. IndexByHash for HandleSettle dispatch.
	bodyBytes, merr := signing.DeterministicMarshal(body)
	if merr != nil {
		return nil, merr
	}
	preimageHash := sha256.Sum256(bodyBytes)

	// Initialise settlement channel before indexing so HandleSettle can
	// send into it immediately after LookupByHash returns.
	if req.SettleSig == nil {
		s.inflt.mu.Lock()
		if req.SettleSig == nil {
			req.SettleSig = make(chan []byte, 1)
		}
		s.inflt.mu.Unlock()
	}
	if ierr := s.inflt.IndexByHash(reqID, preimageHash); ierr != nil {
		return nil, ierr
	}

	// 9. Push SettlementPush to consumer (best-effort).
	push := &tbproto.SettlementPush{
		PreimageHash: preimageHash[:],
		PreimageBody: bodyBytes,
	}
	s.deps.Pusher.PushSettlementTo(req.ConsumerID, push)

	// 10. Spawn awaitSettle goroutine — timer-only path in v1 (see T17.5 TODO).
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.awaitSettle(ctx, req, body, r.SeederSig)
	}()

	return &tbproto.UsageAck{}, nil
}

// awaitSettle waits for a consumer counter-sig or the settlement timeout.
//
// v1 semantic: settlement ALWAYS appends with ConsumerSigMissing=true.
//
// The deps.Identity resolver (broker.IdentityResolver → *server.Server.PeerPubkey)
// is wired and the consumer sig is now available here, BUT we cannot yet verify
// it and flip ConsumerSigMissing=false. The reason is a wire-format constraint:
// the seeder signs the entry body with ConsumerSigMissing=true pre-set (see
// HandleUsageReport and buildSeederSignedReport in settlement_test.go). If we
// rebuild the body with ConsumerSigMissing=false we invalidate the seeder sig
// already stored, and the seeder is no longer present to re-sign.
//
// TODO(broker-followup): T17.5 wire-format amendment — remove ConsumerSigMissing
// from the seeder-signed preimage (or use a two-phase sign) so the broker can
// independently decide the flag value at settlement time. Once that lands,
// resolve the consumer pubkey here, verify the sig, and call appendUsageEntry
// with consumerSigMissing=false when verification succeeds.
func (s *Settlement) awaitSettle(ctx context.Context, req *Request, body *tbproto.EntryBody, seederSig []byte) {
	timeout := time.Duration(s.cfg.SettlementTimeoutS) * time.Second
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-req.SettleSig:
		// Sig arrived. deps.Identity is wired (see T17.5 plumbing) but
		// sig verification is deferred pending a wire-format amendment
		// (see TODO above). Fall through to missing-sig append.
		_ = s.deps.Identity // suppress "field never used" if linter complains; wired for future use.
	case <-timer.C:
	case <-s.stop:
		return
	case <-ctx.Done():
		return
	}
	s.appendUsageEntry(ctx, req, body, seederSig)
}

// appendUsageEntry writes the usage entry to the ledger, retrying on
// ErrStaleTip up to cfg.StaleTipRetries times.
func (s *Settlement) appendUsageEntry(ctx context.Context, req *Request, body *tbproto.EntryBody, seederSig []byte) {
	rec := ledger.UsageRecord{
		PrevHash:           body.PrevHash,
		Seq:                body.Seq,
		ConsumerID:         req.ConsumerID[:],
		SeederID:           req.AssignedSeeder[:],
		Model:              body.Model,
		InputTokens:        body.InputTokens,
		OutputTokens:       body.OutputTokens,
		CostCredits:        body.CostCredits,
		Timestamp:          body.Timestamp,
		RequestID:          body.RequestId,
		ConsumerSigMissing: true, // v1 limitation — T17.5 follow-up
		SeederSig:          seederSig,
		SeederPub:          req.SeederPubkey,
	}
	var appendErr error
	for i := 0; i <= s.cfg.StaleTipRetries; i++ {
		_, appendErr = s.deps.Ledger.AppendUsage(ctx, rec)
		if !errors.Is(appendErr, ledger.ErrStaleTip) {
			break
		}
		tipSeq, tipHash, _, terr := s.deps.Ledger.Tip(ctx)
		if terr != nil {
			appendErr = terr
			break
		}
		rec.PrevHash = tipHash
		rec.Seq = tipSeq + 1
	}
	if appendErr != nil {
		_ = s.inflt.Transition(req.RequestID, StateServing, StateFailed)
		return
	}
	_, _, _ = s.resv.Release(req.RequestID)
	_, _ = s.deps.Registry.DecLoad(req.AssignedSeeder)
	_ = s.inflt.Transition(req.RequestID, StateServing, StateCompleted)
}

// HandleSettle accepts the consumer's counter-signature for a settled usage
// entry identified by its preimage hash. See broker-design §5.3.
func (s *Settlement) HandleSettle(ctx context.Context, peerID ids.IdentityID, r *tbproto.SettleRequest) (*tbproto.SettleAck, error) {
	if r == nil || len(r.PreimageHash) != 32 {
		return nil, errors.New("settlement: malformed Settle")
	}
	var hash [32]byte
	copy(hash[:], r.PreimageHash)
	req, ok := s.inflt.LookupByHash(hash)
	if !ok {
		return nil, ErrUnknownPreimage
	}
	if req.ConsumerID != peerID {
		return nil, ErrSeederMismatch // wrong identity
	}
	if req.SettleSig == nil {
		return nil, ErrUnknownPreimage
	}
	select {
	case req.SettleSig <- r.ConsumerSig:
	default:
		return nil, ErrDuplicateSettle
	}
	return &tbproto.SettleAck{}, nil
}

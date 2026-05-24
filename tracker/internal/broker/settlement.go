package broker

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
	"github.com/token-bay/token-bay/tracker/internal/admission"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
	"github.com/token-bay/token-bay/tracker/internal/ledger/entry"
	"github.com/token-bay/token-bay/tracker/internal/session"
)

// Settlement owns the post-assignment phase of the broker lifecycle:
// usage_report handling, consumer counter-sig waiting, ledger append, and
// reservation release. See broker-design §5.2 and §5.3 for the authoritative
// description of this subsystem.
//
// Construct via OpenSettlement; tear down via Close.
type Settlement struct {
	cfg     config.SettlementConfig
	deps    Deps
	mgr     *session.Manager
	metrics *brokerMetrics
	stop    chan struct{}
	wg      sync.WaitGroup

	// pendingMu guards pending. Each entry tracks per-request settlement
	// coordination state owned by Settlement (the seeder-pushed body bytes
	// the consumer must counter-sign, plus the verification verdict the
	// per-request goroutine reads to decide ConsumerSigMissing on append).
	pendingMu sync.Mutex
	pending   map[[16]byte]*pendingSettle
}

// pendingSettle holds Settlement-internal state for a single in-flight
// settlement. The body bytes are the DeterministicMarshal output of the
// USAGE EntryBody the seeder signed and the consumer is asked to
// counter-sign; verified records whether HandleSettle observed and
// accepted that sig; refused records a verification failure that must
// suppress the timer-fallback append.
type pendingSettle struct {
	body     []byte
	verified bool
	refused  bool
}

// OpenSettlement constructs a ready Settlement. Required deps: Ledger, Pusher.
// Optional: Now (defaults to time.Now). When mgr is nil a fresh session.Manager
// is allocated. Starts the reservation TTL reaper (T19).
func OpenSettlement(cfg config.SettlementConfig, deps Deps, mgr *session.Manager) (*Settlement, error) {
	if deps.Ledger == nil {
		return nil, errors.New("settlement: Ledger required")
	}
	if deps.Pusher == nil {
		return nil, errors.New("settlement: Pusher required")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if mgr == nil {
		mgr = session.New()
	}
	s := &Settlement{
		cfg:     cfg,
		deps:    deps,
		mgr:     mgr,
		metrics: newBrokerMetrics(),
		stop:    make(chan struct{}),
		pending: make(map[[16]byte]*pendingSettle),
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
// Consumer-sig verification is wired via HandleSettle (deps.Identity resolves
// the consumer pubkey). The body the seeder signs is pushed verbatim to the
// consumer, so the consumer's counter-sig is verified over the same bytes.
// Verification verdicts flow through Settlement.pending into appendUsageEntry,
// which sets ConsumerSigMissing=true when the sig is absent or unverifiable
// and false when verified.
func (s *Settlement) HandleUsageReport(ctx context.Context, peerID ids.IdentityID, r *tbproto.UsageReport) (*tbproto.UsageAck, error) {
	if r == nil || len(r.RequestId) != 16 {
		return nil, errors.New("settlement: malformed UsageReport")
	}

	// 1. Lookup inflight request.
	var reqID [16]byte
	copy(reqID[:], r.RequestId)
	req, ok := s.mgr.Inflight.Get(reqID)
	if !ok {
		return nil, session.ErrUnknownRequest
	}

	// 2. State guard — spec §5.2 step 2. Reject any request not in
	// {ASSIGNED, SERVING} with INVALID_STATE before touching the ledger.
	// Catches: a queued/selecting request whose seeder never confirmed, or
	// a terminated request that should not accept a usage_report.
	if req.State != session.StateAssigned && req.State != session.StateServing {
		return nil, ErrInvalidState
	}

	// 3. Seeder must be the caller.
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
	if terr := s.mgr.Inflight.Transition(reqID, session.StateAssigned, session.StateServing, s.deps.Now()); terr != nil {
		if !errors.Is(terr, session.ErrIllegalTransition) {
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
	if _, eerr := s.mgr.Inflight.EnsureSettleSig(reqID); eerr != nil {
		return nil, eerr
	}
	if ierr := s.mgr.Inflight.IndexByHash(reqID, preimageHash); ierr != nil {
		return nil, ierr
	}

	// Record the preimage body so HandleSettle can verify the consumer's
	// counter-sig over the exact bytes the seeder vouched for and pushed.
	s.putPending(reqID, bodyBytes)

	// 9. Push SettlementPush to consumer (best-effort).
	push := &tbproto.SettlementPush{
		PreimageHash: preimageHash[:],
		PreimageBody: bodyBytes,
	}
	s.deps.Pusher.PushSettlementTo(req.ConsumerID, push)

	// 10. Spawn awaitSettle goroutine — handles consumer-sig arrival or timeout.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.awaitSettle(ctx, req, body, r.SeederSig)
	}()

	return &tbproto.UsageAck{}, nil
}

// putPending records the preimage body bytes for an in-flight settlement
// and initialises a fresh verification verdict. Called by HandleUsageReport
// after IndexByHash so HandleSettle observes the body before the consumer
// can possibly counter-sign.
func (s *Settlement) putPending(reqID [16]byte, body []byte) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	s.pending[reqID] = &pendingSettle{body: body}
}

// getPending returns the pending settlement entry for reqID, or nil if
// none. Safe for concurrent use.
func (s *Settlement) getPending(reqID [16]byte) *pendingSettle {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	return s.pending[reqID]
}

// dropPending removes the pending settlement entry for reqID. Called after
// the per-request goroutine has decided on its append outcome.
func (s *Settlement) dropPending(reqID [16]byte) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	delete(s.pending, reqID)
}

// awaitSettle waits for a consumer counter-sig or the settlement timeout
// and dispatches the verdict to appendUsageEntry. Three terminating
// branches:
//
//   - sig signal: HandleSettle observed the consumer's counter-sig. The
//     verdict on Settlement.pending says whether verification succeeded
//     (ConsumerSigMissing=false), or the pubkey was unknown / verification
//     was skipped (ConsumerSigMissing=true).
//   - timer expiry: no sig arrived; if HandleSettle did not previously
//     refuse a tampered sig, append with ConsumerSigMissing=true.
//   - stop / ctx cancel: tracker shutting down; abandon the request.
//
// A refused-sig verdict (verification failure) suppresses the timer path
// so no entry is written — spec §5.2 "Verify fail: no ledger touch."
func (s *Settlement) awaitSettle(ctx context.Context, req *session.Request, body *tbproto.EntryBody, seederSig []byte) {
	defer s.dropPending(req.RequestID)

	timeout := time.Duration(s.cfg.SettlementTimeoutS) * time.Second
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-req.SettleSig:
		p := s.getPending(req.RequestID)
		if p != nil && p.refused {
			// HandleSettle observed a tampered sig and rejected it;
			// no ledger entry should be written.
			return
		}
		consumerSigMissing := p == nil || !p.verified
		s.appendUsageEntry(ctx, req, body, seederSig, consumerSigMissing)
	case <-timer.C:
		p := s.getPending(req.RequestID)
		if p != nil && p.refused {
			return
		}
		s.appendUsageEntry(ctx, req, body, seederSig, true)
	case <-s.stop:
		return
	case <-ctx.Done():
		return
	}
}

// appendUsageEntry writes the usage entry to the ledger, retrying on
// ErrStaleTip up to cfg.StaleTipRetries times. consumerSigMissing flows
// from awaitSettle's verdict and is stored on the entry verbatim.
func (s *Settlement) appendUsageEntry(ctx context.Context, req *session.Request, body *tbproto.EntryBody, seederSig []byte, consumerSigMissing bool) {
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
		ConsumerSigMissing: consumerSigMissing,
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
		_ = s.mgr.Inflight.Transition(req.RequestID, session.StateServing, session.StateFailed, s.deps.Now())
		return
	}
	_, _, _ = s.mgr.Reservations.Release(req.RequestID)
	_, _ = s.deps.Registry.DecLoad(req.AssignedSeeder)
	_ = s.mgr.Inflight.Transition(req.RequestID, session.StateServing, session.StateCompleted, s.deps.Now())
	if s.deps.Reputation != nil {
		var flags uint32
		if consumerSigMissing {
			flags = 1 // bit 0 = consumer_sig_missing
		}
		s.deps.Reputation.OnLedgerEvent(admission.LedgerEvent{
			Kind:        admission.LedgerEventSettlement,
			ConsumerID:  req.ConsumerID,
			SeederID:    req.AssignedSeeder,
			CostCredits: rec.CostCredits,
			Flags:       flags,
			Timestamp:   time.Unix(int64(rec.Timestamp), 0), //nolint:gosec // G115: always positive
		})
	}
}

// HandleSettle accepts the consumer's counter-signature for a settled usage
// entry identified by its preimage hash. See broker-design §5.2 / §5.3.
//
// When the consumer pubkey is resolvable via deps.Identity, the sig is
// verified over the preimage body bytes recorded by HandleUsageReport.
// Outcomes:
//
//   - verified: dispatch the sig and mark the request's pending entry so
//     awaitSettle appends with ConsumerSigMissing=false.
//   - tampered: return ErrConsumerSig, emit a "refused_consumer_sig" audit
//     entry, and suppress the timer-fallback append (no ledger touch).
//   - pubkey unknown (Identity nil, peer not connected, or pending entry
//     missing): bump ConsumerPubkeyUnknown, dispatch the sig, and let
//     awaitSettle append with ConsumerSigMissing=true.
func (s *Settlement) HandleSettle(_ context.Context, peerID ids.IdentityID, r *tbproto.SettleRequest) (*tbproto.SettleAck, error) {
	if r == nil || len(r.PreimageHash) != 32 {
		return nil, errors.New("settlement: malformed Settle")
	}
	var hash [32]byte
	copy(hash[:], r.PreimageHash)
	req, ok := s.mgr.Inflight.LookupByHash(hash)
	if !ok {
		return nil, ErrUnknownPreimage
	}
	if req.ConsumerID != peerID {
		return nil, ErrSeederMismatch // wrong identity
	}
	if req.SettleSig == nil {
		return nil, ErrUnknownPreimage
	}

	// Resolve the consumer pubkey and verify the sig against the
	// preimage body the seeder pushed (and that we recorded in
	// putPending). Missing resolver / missing pubkey / missing pending
	// entry all degrade to "unverifiable" — bump the counter and let
	// awaitSettle append with ConsumerSigMissing=true.
	verdict := s.verifyConsumerSig(req, r.ConsumerSig)
	switch verdict {
	case sigVerified:
		// fall through to dispatch
	case sigRefused:
		s.deps.Logger.Info().
			Str("event", "refused_consumer_sig").
			Str("request_id", hex.EncodeToString(req.RequestID[:])).
			Str("consumer_id", hex.EncodeToString(req.ConsumerID[:])).
			Msg("")
		return nil, ErrConsumerSig
	case sigUnverifiable:
		s.metrics.ConsumerPubkeyUnknown.Inc()
		// fall through to dispatch; awaitSettle will see verified=false.
	}

	select {
	case req.SettleSig <- r.ConsumerSig:
	default:
		return nil, ErrDuplicateSettle
	}
	return &tbproto.SettleAck{}, nil
}

// sigVerdict is the per-call result of verifyConsumerSig.
type sigVerdict int

const (
	sigVerified sigVerdict = iota
	sigRefused
	sigUnverifiable
)

// verifyConsumerSig classifies the consumer's counter-sig and, on
// success, marks the pending entry so awaitSettle can append with
// ConsumerSigMissing=false. On verification failure it marks the entry
// refused so awaitSettle's timer fallback also skips the append.
func (s *Settlement) verifyConsumerSig(req *session.Request, sig []byte) sigVerdict {
	if s.deps.Identity == nil {
		return sigUnverifiable
	}
	p := s.getPending(req.RequestID)
	if p == nil || len(p.body) == 0 {
		return sigUnverifiable
	}
	pub, ok := s.deps.Identity.PeerPubkey(req.ConsumerID)
	if !ok || len(pub) != ed25519.PublicKeySize {
		return sigUnverifiable
	}
	if !ed25519.Verify(pub, p.body, sig) {
		s.pendingMu.Lock()
		p.refused = true
		s.pendingMu.Unlock()
		return sigRefused
	}
	s.pendingMu.Lock()
	p.verified = true
	s.pendingMu.Unlock()
	return sigVerified
}

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
	// coordination state owned by Settlement (the usage-assertion the
	// seeder signed and the consumer must counter-sign, plus the
	// verification verdict the per-request goroutine reads to decide
	// ConsumerSigMissing on append).
	pendingMu sync.Mutex
	pending   map[[16]byte]*pendingSettle
}

// pendingSettle holds Settlement-internal state for a single in-flight
// settlement. assertion is the sequencing-independent usage-assertion the
// seeder signed (shared/signing); preSig is its canonical preimage bytes —
// exactly what was pushed to the consumer to counter-sign. verified records
// whether HandleSettle observed and accepted the consumer's counter-sig
// (consumerSig/consumerPub then carry the verified sig + resolved pubkey
// for the ledger record); refused records a verification failure that must
// suppress the timer-fallback append.
type pendingSettle struct {
	assertion signing.UsageAssertion
	preSig    []byte

	verified    bool
	refused     bool
	consumerSig []byte
	consumerPub ed25519.PublicKey
}

// OpenSettlement constructs a ready Settlement. Required deps: Ledger, Pusher.
// Optional: Now (defaults to time.Now). When mgr is nil a fresh session.Manager
// is allocated. Starts the reservation TTL reaper (T19).
func OpenSettlement(cfg config.SettlementConfig, deps Deps, mgr *session.Manager) (*Settlement, error) {
	return openSettlementWithMetrics(cfg, deps, mgr, newBrokerMetrics())
}

// openSettlementWithMetrics is the internal constructor that lets Subsystems
// share a single brokerMetrics instance across Broker + Settlement.
func openSettlementWithMetrics(cfg config.SettlementConfig, deps Deps, mgr *session.Manager, metrics *brokerMetrics) (*Settlement, error) {
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
	if metrics == nil {
		metrics = newBrokerMetrics()
	}
	s := &Settlement{
		cfg:     cfg,
		deps:    deps,
		mgr:     mgr,
		metrics: metrics,
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
// Both participants sign the sequencing-independent usage-assertion
// (shared/signing.UsageAssertion): the seeder's UsageReport sig is verified
// over it here (with the per-offer ephemeral key), and its canonical bytes
// are pushed verbatim to the consumer, whose counter-sig HandleSettle
// verifies over the same bytes (deps.Identity resolves the consumer's mTLS
// identity pubkey). Verification verdicts flow through Settlement.pending
// into awaitSettle, which appends with ConsumerSigMissing=true when the sig
// is absent or unverifiable and carries the verified sig + pubkey when not.
//
// Settlement authorization is single-use per request_id: a report arriving
// while a settlement for the same request is already in flight returns
// ErrDuplicateUsageReport (the putPending gate), and one arriving after the
// request reached a terminal state returns ErrInvalidState (the state
// guard). The ledger enforces the same invariant independently via
// ledger.ErrUsageRequestExists.
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

	// 6. Read the current tip — the eventual ledger append needs fresh
	// (prev_hash, seq) sequencing, but participant signatures no longer
	// cover it.
	tipSeq, tipHash, hasTip, terr := s.deps.Ledger.Tip(ctx)
	if terr != nil {
		return nil, terr
	}
	prevHash := tipHash
	nextSeq := tipSeq + 1
	if !hasTip {
		prevHash = make([]byte, 32)
		nextSeq = 1
	}

	// 7. Build the sequencing-independent usage-assertion and verify the
	// seeder's sig over it. The seeder signs with its per-offer ephemeral
	// key (req.SeederPubkey).
	assertion := signing.UsageAssertion{
		RequestID:    r.RequestId,
		ConsumerID:   req.ConsumerID[:],
		SeederID:     req.AssignedSeeder[:],
		Model:        r.Model,
		InputTokens:  r.InputTokens,
		OutputTokens: r.OutputTokens,
		CostCredits:  actualCost,
	}
	preSig, perr := signing.CanonicalUsageAssertionPreSig(assertion)
	if perr != nil {
		return nil, perr
	}
	if !signing.VerifyUsageAssertion(req.SeederPubkey, assertion, r.SeederSig) {
		return nil, ErrSeederSigInvalid
	}

	// 8. IndexByHash for HandleSettle dispatch. The settlement preimage is
	// the canonical usage-assertion bytes.
	preimageHash := sha256.Sum256(preSig)

	// Initialise settlement channel before indexing so HandleSettle can
	// send into it immediately after LookupByHash returns.
	if _, eerr := s.mgr.Inflight.EnsureSettleSig(reqID); eerr != nil {
		return nil, eerr
	}
	if ierr := s.mgr.Inflight.IndexByHash(reqID, preimageHash); ierr != nil {
		return nil, ierr
	}

	// Record the assertion + preimage bytes so HandleSettle can verify the
	// consumer's counter-sig over the exact bytes the seeder vouched for
	// and that were pushed. putPending is the atomic check-and-insert
	// dedupe gate: a pre-existing entry means a settlement for this
	// request is already in flight, so this report is a duplicate (seeder
	// retry or replay) and must not spawn a second awaitSettle/append.
	if !s.putPending(reqID, assertion, preSig) {
		return nil, ErrDuplicateUsageReport
	}

	// 9. Push SettlementPush to consumer (best-effort).
	push := &tbproto.SettlementPush{
		PreimageHash: preimageHash[:],
		PreimageBody: preSig,
	}
	s.deps.Pusher.PushSettlementTo(req.ConsumerID, push)

	// 10. Spawn awaitSettle goroutine — handles consumer-sig arrival or
	// timeout. The UsageRecord is complete except for the consumer-sig
	// verdict fields, which awaitSettle fills before appending.
	now := s.deps.Now()
	rec := ledger.UsageRecord{
		PrevHash:     prevHash,
		Seq:          nextSeq,
		ConsumerID:   req.ConsumerID[:],
		SeederID:     req.AssignedSeeder[:],
		Model:        r.Model,
		InputTokens:  r.InputTokens,
		OutputTokens: r.OutputTokens,
		CostCredits:  actualCost,
		Timestamp:    uint64(now.Unix()), //nolint:gosec // G115: always positive
		RequestID:    r.RequestId,
		SeederSig:    r.SeederSig,
		SeederPub:    req.SeederPubkey,
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.awaitSettle(ctx, req, rec)
	}()

	return &tbproto.UsageAck{}, nil
}

// putPending records the usage-assertion + its canonical preimage bytes for
// an in-flight settlement and initialises a fresh verification verdict.
// Called by HandleUsageReport after IndexByHash so HandleSettle observes
// the assertion before the consumer can possibly counter-sign.
//
// The check-and-insert is atomic under pendingMu: it returns false — and
// leaves the existing entry untouched — when a settlement for reqID is
// already pending, so two racing reports can never both win the gate. The
// entry lives until awaitSettle's dropPending, which runs strictly after
// the request leaves SERVING on append success/failure; a duplicate report
// arriving after that is rejected by HandleUsageReport's state guard.
func (s *Settlement) putPending(reqID [16]byte, assertion signing.UsageAssertion, preSig []byte) bool {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if _, exists := s.pending[reqID]; exists {
		return false
	}
	s.pending[reqID] = &pendingSettle{assertion: assertion, preSig: preSig}
	return true
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

// pendingVerdict snapshots the verification verdict for reqID under
// pendingMu. sig/pub are only non-nil when verified — they carry the
// consumer's counter-sig over the usage-assertion and the resolved
// consumer pubkey for the ledger record.
func (s *Settlement) pendingVerdict(reqID [16]byte) (verified, refused bool, sig []byte, pub ed25519.PublicKey) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	p := s.pending[reqID]
	if p == nil {
		return false, false, nil, nil
	}
	return p.verified, p.refused, p.consumerSig, p.consumerPub
}

// awaitSettle waits for a consumer counter-sig or the settlement timeout
// and dispatches the verdict to appendUsageEntry. Three terminating
// branches:
//
//   - sig signal: HandleSettle observed the consumer's counter-sig. The
//     verdict on Settlement.pending says whether verification succeeded —
//     the record then carries the verified sig + resolved pubkey with
//     ConsumerSigMissing=false — or the pubkey was unknown / verification
//     was skipped (ConsumerSigMissing=true, no sig).
//   - timer expiry: no sig arrived; if HandleSettle did not previously
//     refuse a tampered sig, append with ConsumerSigMissing=true.
//   - stop / ctx cancel: tracker shutting down; abandon the request.
//
// A refused-sig verdict (verification failure) suppresses the timer path
// so no entry is written — spec §5.2 "Verify fail: no ledger touch."
func (s *Settlement) awaitSettle(ctx context.Context, req *session.Request, rec ledger.UsageRecord) {
	defer s.dropPending(req.RequestID)

	timeout := time.Duration(s.cfg.SettlementTimeoutS) * time.Second
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-req.SettleSig:
		verified, refused, sig, pub := s.pendingVerdict(req.RequestID)
		if refused {
			// HandleSettle observed a tampered sig and rejected it;
			// no ledger entry should be written.
			return
		}
		if verified {
			rec.ConsumerSigMissing = false
			rec.ConsumerSig = sig
			rec.ConsumerPub = pub
		} else {
			rec.ConsumerSigMissing = true
		}
		s.appendUsageEntry(ctx, req, rec)
	case <-timer.C:
		_, refused, _, _ := s.pendingVerdict(req.RequestID)
		if refused {
			return
		}
		rec.ConsumerSigMissing = true
		s.appendUsageEntry(ctx, req, rec)
	case <-s.stop:
		return
	case <-ctx.Done():
		return
	}
}

// appendUsageEntry writes the usage entry to the ledger, retrying on
// ErrStaleTip up to cfg.StaleTipRetries times. Because the participant
// sigs are over the sequencing-independent usage-assertion, a retry only
// refreshes (prev_hash, seq) — the sigs stay valid. rec arrives fully
// populated from awaitSettle, consumer-sig verdict included.
func (s *Settlement) appendUsageEntry(ctx context.Context, req *session.Request, rec ledger.UsageRecord) {
	var appendErr error
	for i := 0; i <= s.cfg.StaleTipRetries; i++ {
		_, appendErr = s.deps.Ledger.AppendUsage(ctx, rec)
		if !errors.Is(appendErr, ledger.ErrStaleTip) {
			break
		}
		s.metrics.StaleTipRetries.Inc()
		tipSeq, tipHash, _, terr := s.deps.Ledger.Tip(ctx)
		if terr != nil {
			appendErr = terr
			break
		}
		rec.PrevHash = tipHash
		rec.Seq = tipSeq + 1
	}
	if appendErr != nil {
		s.metrics.LedgerAppendFailure.Inc()
		_ = s.mgr.Inflight.Transition(req.RequestID, session.StateServing, session.StateFailed, s.deps.Now())
		return
	}
	_, _, _ = s.mgr.Reservations.Release(req.RequestID)
	_, _ = s.deps.Registry.DecLoad(req.AssignedSeeder)
	_ = s.mgr.Inflight.Transition(req.RequestID, session.StateServing, session.StateCompleted, s.deps.Now())
	if s.deps.Reputation != nil {
		var flags uint32
		if rec.ConsumerSigMissing {
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
// verified over the usage-assertion recorded by HandleUsageReport — the
// same bytes the seeder signed and the tracker pushed. Outcomes:
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

// verifyConsumerSig classifies the consumer's counter-sig over the
// usage-assertion and, on success, stores the verified sig + resolved
// pubkey on the pending entry so awaitSettle can append with
// ConsumerSigMissing=false. On verification failure it marks the entry
// refused so awaitSettle's timer fallback also skips the append. The
// consumer signs with its mTLS identity key (deps.Identity.PeerPubkey).
func (s *Settlement) verifyConsumerSig(req *session.Request, sig []byte) sigVerdict {
	if s.deps.Identity == nil {
		return sigUnverifiable
	}
	p := s.getPending(req.RequestID)
	if p == nil || len(p.preSig) == 0 {
		return sigUnverifiable
	}
	pub, ok := s.deps.Identity.PeerPubkey(req.ConsumerID)
	if !ok || len(pub) != ed25519.PublicKeySize {
		return sigUnverifiable
	}
	if !signing.VerifyUsageAssertion(pub, p.assertion, sig) {
		s.pendingMu.Lock()
		p.refused = true
		s.pendingMu.Unlock()
		return sigRefused
	}
	s.pendingMu.Lock()
	p.verified = true
	p.consumerSig = sig
	p.consumerPub = pub
	s.pendingMu.Unlock()
	return sigVerified
}

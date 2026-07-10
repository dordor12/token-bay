// Package federation: cross-region credit transfer coordinator.
//
// transferCoordinator owns:
//   - the destination-side pending-request map keyed by Nonce
//   - the source-side issued-proof replay cache keyed by Nonce
//   - the destination-side completed-transfer cache keyed by Nonce
//
// All three are in-memory; the durable double-spend backstops are the
// ledger's on-chain per-kind single-use transfer ref checks
// (ledger.ErrTransferRefExists), which hold across restarts: the
// TRANSFER_OUT check at the source (double-debit) and the TRANSFER_IN
// check at the destination (double-credit). Both are needed — a source
// answers a replayed request from its warm issued cache without
// re-entering the ledger, so only the destination's own on-chain check
// bounds a dest-restart replay.
package federation

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"time"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"google.golang.org/protobuf/proto"
)

type transferCoordinatorCfg struct {
	MyTrackerID    ids.TrackerID
	MyPriv         ed25519.PrivateKey
	Ledger         LedgerHooks
	IssuedCap      int
	Now            func() time.Time
	PeerPubKey     func(ids.TrackerID) (ed25519.PublicKey, bool)
	Send           func(context.Context, ids.TrackerID, fed.Kind, []byte) error
	MetricsCounter func(name string)
}

type issuedProof struct {
	payload []byte
	at      time.Time
}

// completedTransfer is one destination-side finished transfer, cached so
// a replayed StartTransfer with the same nonce returns the original
// result without a second proof round-trip or a second AppendTransferIn.
type completedTransfer struct {
	out StartTransferOutput
	at  time.Time
}

type pendingResp struct {
	ch    chan *fed.TransferProof
	rejCh chan *fed.TransferReject // slice 13: signed negative-ack from source
}

// transferCoordinator is constructed by Federation.Open and lives for
// the federation subsystem's lifetime.
type transferCoordinator struct {
	cfg transferCoordinatorCfg

	mu        sync.Mutex
	issued    map[[32]byte]issuedProof       // source-side replay cache
	pending   map[[32]byte]pendingResp       // dest-side in-flight
	completed map[[32]byte]completedTransfer // dest-side finished transfers
}

func newTransferCoordinator(cfg transferCoordinatorCfg) *transferCoordinator {
	if cfg.IssuedCap <= 0 {
		cfg.IssuedCap = 4096
	}
	if cfg.MetricsCounter == nil {
		cfg.MetricsCounter = func(string) {}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &transferCoordinator{
		cfg:       cfg,
		issued:    make(map[[32]byte]issuedProof),
		pending:   make(map[[32]byte]pendingResp),
		completed: make(map[[32]byte]completedTransfer),
	}
}

// StartTransferInput is what the api handler hands to federation.
type StartTransferInput struct {
	SourceTrackerID ids.TrackerID
	IdentityID      ids.IdentityID
	Amount          uint64
	Nonce           [32]byte
	ConsumerSig     []byte
	ConsumerPub     ed25519.PublicKey
	Timestamp       uint64
}

// StartTransferOutput is what federation returns after a successful
// end-to-end proof exchange.
type StartTransferOutput struct {
	SourceChainTipHash [32]byte
	SourceSeq          uint64
	SourceTrackerSig   []byte
}

// OnRequest handles an inbound KIND_TRANSFER_PROOF_REQUEST envelope at
// the source tracker. fromPeer is the sender's TrackerID (already
// verified against env.sender_id by the dispatcher's envelope-sig
// check).
func (tc *transferCoordinator) OnRequest(ctx context.Context, env *fed.Envelope, fromPeer ids.TrackerID) {
	if tc.cfg.Ledger == nil {
		tc.cfg.MetricsCounter("transfer_request_received_disabled")
		return
	}
	req := &fed.TransferProofRequest{}
	if err := proto.Unmarshal(env.Payload, req); err != nil {
		tc.cfg.MetricsCounter("transfer_request_shape")
		return
	}
	if err := fed.ValidateTransferProofRequest(req); err != nil {
		tc.cfg.MetricsCounter("transfer_request_shape")
		return
	}
	myID := tc.cfg.MyTrackerID.Bytes()
	if !equalID(req.SourceTrackerId, myID[:]) {
		tc.cfg.MetricsCounter("transfer_request_misrouted")
		return
	}
	dst := fromPeer.Bytes()
	if !equalID(req.DestTrackerId, dst[:]) {
		tc.cfg.MetricsCounter("transfer_request_dest_mismatch")
		return
	}
	canonical, err := fed.CanonicalTransferProofRequestPreSig(req)
	if err != nil {
		tc.cfg.MetricsCounter("transfer_request_canonical")
		return
	}
	if !ed25519.Verify(req.ConsumerPub, canonical, req.ConsumerSig) {
		tc.cfg.MetricsCounter("transfer_request_consumer_sig")
		return
	}

	var nonceArr [32]byte
	copy(nonceArr[:], req.Nonce)

	tc.mu.Lock()
	if cached, ok := tc.issued[nonceArr]; ok {
		payload := cached.payload
		tc.mu.Unlock()
		_ = tc.cfg.Send(ctx, fromPeer, fed.Kind_KIND_TRANSFER_PROOF, payload)
		tc.cfg.MetricsCounter("transfer_request_replayed")
		return
	}
	tc.mu.Unlock()

	var identityArr [32]byte
	copy(identityArr[:], req.IdentityId)
	// The tracker ids ride along so the ledger can rebuild the exact
	// canonical intent the consumer signed; without them the preimage
	// cannot byte-match and the append fails "consumer_sig invalid".
	var srcArr, dstArr [32]byte
	copy(srcArr[:], req.SourceTrackerId)
	copy(dstArr[:], req.DestTrackerId)
	out, err := tc.cfg.Ledger.AppendTransferOut(ctx, TransferOutHookIn{
		IdentityID:      identityArr,
		Amount:          req.Amount,
		Timestamp:       req.Timestamp,
		TransferRef:     nonceArr,
		SourceTrackerID: srcArr,
		DestTrackerID:   dstArr,
		ConsumerSig:     req.ConsumerSig,
		ConsumerPub:     req.ConsumerPub,
	})
	if err != nil {
		// Slice 13: emit a signed TransferReject so the destination's
		// pending StartTransfer fails fast with a typed reason instead
		// of waiting for the request timeout. The reason string is
		// derived from the ledger sentinel and capped at 64 bytes by
		// the validator.
		tc.emitTransferReject(ctx, fromPeer, req, err.Error())
		tc.cfg.MetricsCounter("transfer_request_ledger_err")
		return
	}

	proof := &fed.TransferProof{
		SourceTrackerId:    req.SourceTrackerId,
		DestTrackerId:      req.DestTrackerId,
		IdentityId:         req.IdentityId,
		Amount:             req.Amount,
		Nonce:              req.Nonce,
		SourceChainTipHash: out.ChainTipHash[:],
		SourceSeq:          out.Seq,
		Timestamp:          req.Timestamp,
	}
	cb, err := fed.CanonicalTransferProofPreSig(proof)
	if err != nil {
		tc.cfg.MetricsCounter("transfer_proof_canonical")
		return
	}
	proof.SourceTrackerSig = ed25519.Sign(tc.cfg.MyPriv, cb)
	payload, err := proto.Marshal(proof)
	if err != nil {
		tc.cfg.MetricsCounter("transfer_proof_marshal")
		return
	}

	tc.mu.Lock()
	tc.cacheIssuedLocked(nonceArr, payload)
	tc.mu.Unlock()

	if err := tc.cfg.Send(ctx, fromPeer, fed.Kind_KIND_TRANSFER_PROOF, payload); err != nil {
		tc.cfg.MetricsCounter("transfer_proof_send_err")
		return
	}
	tc.cfg.MetricsCounter("transfers_minted")
}

// cacheIssuedLocked stores payload keyed by nonce, evicting oldest if
// the cap is exceeded. Caller must hold tc.mu.
func (tc *transferCoordinator) cacheIssuedLocked(nonce [32]byte, payload []byte) {
	if len(tc.issued) >= tc.cfg.IssuedCap {
		var oldestKey [32]byte
		var oldestAt time.Time
		first := true
		for k, v := range tc.issued {
			if first || v.at.Before(oldestAt) {
				oldestKey = k
				oldestAt = v.at
				first = false
			}
		}
		delete(tc.issued, oldestKey)
	}
	tc.issued[nonce] = issuedProof{payload: payload, at: tc.cfg.Now()}
}

// emitTransferReject builds, signs, and sends a KIND_TRANSFER_REJECT
// envelope back to the destination peer that originated the request.
// Slice 13. Errors during build/sign are logged via the metrics
// counter and do not block the caller — the request is already failing.
func (tc *transferCoordinator) emitTransferReject(ctx context.Context, dest ids.TrackerID, req *fed.TransferProofRequest, reason string) {
	if len(reason) > fed.MaxTransferRejectReasonLen {
		reason = reason[:fed.MaxTransferRejectReasonLen]
	}
	rej := &fed.TransferReject{
		SourceTrackerId: req.SourceTrackerId,
		DestTrackerId:   req.DestTrackerId,
		Nonce:           req.Nonce,
		Reason:          reason,
		Timestamp:       uint64(tc.cfg.Now().Unix()), //nolint:gosec // Unix() ≥ 0 for any post-epoch timestamp
	}
	cb, err := fed.CanonicalTransferRejectPreSig(rej)
	if err != nil {
		tc.cfg.MetricsCounter("transfer_reject_canonical")
		return
	}
	rej.SourceTrackerSig = ed25519.Sign(tc.cfg.MyPriv, cb)
	if err := fed.ValidateTransferReject(rej); err != nil {
		tc.cfg.MetricsCounter("transfer_reject_validate")
		return
	}
	payload, err := proto.Marshal(rej)
	if err != nil {
		tc.cfg.MetricsCounter("transfer_reject_marshal")
		return
	}
	if err := tc.cfg.Send(ctx, dest, fed.Kind_KIND_TRANSFER_REJECT, payload); err != nil {
		tc.cfg.MetricsCounter("transfer_reject_send_err")
		return
	}
	tc.cfg.MetricsCounter("transfer_reject_sent")
}

// IssueTransferReversal (slice 14) builds, signs, and forwards a
// TransferReversal envelope from the destination (this tracker) toward
// the source. Used by the admin API after operator review of a
// transfer-out that never settled at the destination after the §4.3
// 24h window. The source side, on receipt, MAY refund — operator
// policy decision; v1 is informational + auditable, no automatic
// refund. Returns the signed wire bytes for audit logging.
func (tc *transferCoordinator) IssueTransferReversal(
	ctx context.Context, source ids.TrackerID, nonce [32]byte, evidence string,
) ([]byte, error) {
	srcID := source.Bytes()
	myID := tc.cfg.MyTrackerID.Bytes()
	rev := &fed.TransferReversal{
		SourceTrackerId: srcID[:],
		DestTrackerId:   myID[:],
		Nonce:           nonce[:],
		Evidence:        evidence,
		Timestamp:       uint64(tc.cfg.Now().Unix()), //nolint:gosec
	}
	cb, err := fed.CanonicalTransferReversalPreSig(rev)
	if err != nil {
		return nil, fmt.Errorf("federation: canonical reversal: %w", err)
	}
	rev.DestTrackerSig = ed25519.Sign(tc.cfg.MyPriv, cb)
	if err := fed.ValidateTransferReversal(rev); err != nil {
		return nil, fmt.Errorf("federation: validate reversal: %w", err)
	}
	payload, err := proto.Marshal(rev)
	if err != nil {
		return nil, fmt.Errorf("federation: marshal reversal: %w", err)
	}
	if err := tc.cfg.Send(ctx, source, fed.Kind_KIND_TRANSFER_REVERSAL, payload); err != nil {
		return nil, fmt.Errorf("federation: send reversal: %w", err)
	}
	tc.cfg.MetricsCounter("transfer_reversal_sent")
	return payload, nil
}

// OnTransferReversal is the source-side dispatch hook. Validates the
// envelope (sig + shape). v1 is informational only — emits a metric +
// log line. Future operator-driven refund automation will hang off
// this hook.
func (tc *transferCoordinator) OnTransferReversal(_ context.Context, env *fed.Envelope, fromPeer ids.TrackerID) {
	rev := &fed.TransferReversal{}
	if err := proto.Unmarshal(env.Payload, rev); err != nil {
		tc.cfg.MetricsCounter("transfer_reversal_shape")
		return
	}
	if err := fed.ValidateTransferReversal(rev); err != nil {
		tc.cfg.MetricsCounter("transfer_reversal_shape")
		return
	}
	pub, ok := tc.cfg.PeerPubKey(fromPeer)
	if !ok {
		tc.cfg.MetricsCounter("transfer_reversal_unknown_issuer")
		return
	}
	cb, err := fed.CanonicalTransferReversalPreSig(rev)
	if err != nil {
		tc.cfg.MetricsCounter("transfer_reversal_canonical")
		return
	}
	if !ed25519.Verify(pub, cb, rev.DestTrackerSig) {
		tc.cfg.MetricsCounter("transfer_reversal_sig")
		return
	}
	tc.cfg.MetricsCounter("transfer_reversal_received")
}

// OnReject is the dest-side dispatch hook. Validates the envelope and
// hands the parsed reject to the pending StartTransfer call via rejCh.
// If no pending call matches the nonce, the reject is dropped silently
// (the destination's StartTransfer call has already returned via
// ctx.Done or an out-of-band path).
func (tc *transferCoordinator) OnReject(_ context.Context, env *fed.Envelope, fromPeer ids.TrackerID) {
	rej := &fed.TransferReject{}
	if err := proto.Unmarshal(env.Payload, rej); err != nil {
		tc.cfg.MetricsCounter("transfer_reject_shape")
		return
	}
	if err := fed.ValidateTransferReject(rej); err != nil {
		tc.cfg.MetricsCounter("transfer_reject_shape")
		return
	}
	// Verify the source's signature using the peer's known pubkey.
	srcPub, ok := tc.cfg.PeerPubKey(fromPeer)
	if !ok {
		tc.cfg.MetricsCounter("transfer_reject_unknown_issuer")
		return
	}
	cb, err := fed.CanonicalTransferRejectPreSig(rej)
	if err != nil {
		tc.cfg.MetricsCounter("transfer_reject_canonical")
		return
	}
	if !ed25519.Verify(srcPub, cb, rej.SourceTrackerSig) {
		tc.cfg.MetricsCounter("transfer_reject_sig")
		return
	}
	var nonceArr [32]byte
	copy(nonceArr[:], rej.Nonce)

	tc.mu.Lock()
	p, ok := tc.pending[nonceArr]
	tc.mu.Unlock()
	if !ok {
		tc.cfg.MetricsCounter("transfer_reject_no_pending")
		return
	}
	select {
	case p.rejCh <- rej:
		tc.cfg.MetricsCounter("transfer_reject_delivered")
	default:
		// already resolved by another path
	}
}

func equalID(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// StartTransfer is the destination-side entry point. The api handler
// (or its test stub) calls it after collecting the consumer-signed
// request. Blocks until the source peer responds with a TRANSFER_PROOF,
// ctx is canceled, or — when the upper Federation.StartTransfer wrapper
// applies a TransferTimeout — the deadline elapses.
//
// On success: appends a transfer_in to the local ledger, sends a
// signed TRANSFER_APPLIED back to the source (best-effort), and returns
// the proof.
func (tc *transferCoordinator) StartTransfer(ctx context.Context, in StartTransferInput) (StartTransferOutput, error) {
	if tc.cfg.Ledger == nil {
		return StartTransferOutput{}, ErrTransferDisabled
	}

	// Destination-side idempotency: a replayed StartTransfer for an
	// already-completed nonce returns the cached result — no second proof
	// round-trip, no second AppendTransferIn (no double-credit).
	tc.mu.Lock()
	if c, ok := tc.completed[in.Nonce]; ok {
		tc.mu.Unlock()
		tc.cfg.MetricsCounter("transfer_start_replayed_completed")
		return c.out, nil
	}
	tc.mu.Unlock()

	myID := tc.cfg.MyTrackerID.Bytes()
	srcID := in.SourceTrackerID.Bytes()
	idArr := [32]byte(in.IdentityID)

	req := &fed.TransferProofRequest{
		SourceTrackerId: srcID[:],
		DestTrackerId:   myID[:],
		IdentityId:      idArr[:],
		Amount:          in.Amount,
		Nonce:           in.Nonce[:],
		ConsumerSig:     in.ConsumerSig,
		ConsumerPub:     in.ConsumerPub,
		Timestamp:       in.Timestamp,
	}
	if err := fed.ValidateTransferProofRequest(req); err != nil {
		return StartTransferOutput{}, fmt.Errorf("federation: %w", err)
	}
	payload, err := proto.Marshal(req)
	if err != nil {
		return StartTransferOutput{}, fmt.Errorf("federation: marshal request: %w", err)
	}

	ch := make(chan *fed.TransferProof, 1)
	rejCh := make(chan *fed.TransferReject, 1)
	tc.mu.Lock()
	if _, exists := tc.pending[in.Nonce]; exists {
		tc.mu.Unlock()
		return StartTransferOutput{}, errors.New("federation: duplicate StartTransfer nonce in flight")
	}
	tc.pending[in.Nonce] = pendingResp{ch: ch, rejCh: rejCh}
	tc.mu.Unlock()
	defer func() {
		tc.mu.Lock()
		delete(tc.pending, in.Nonce)
		tc.mu.Unlock()
	}()

	if err := tc.cfg.Send(ctx, in.SourceTrackerID, fed.Kind_KIND_TRANSFER_PROOF_REQUEST, payload); err != nil {
		return StartTransferOutput{}, err
	}
	tc.cfg.MetricsCounter("transfer_request_sent")

	var proof *fed.TransferProof
	select {
	case proof = <-ch:
	case rej := <-rejCh:
		// Slice 13: source-side rejection. Surface the reason verbatim
		// (validator capped it to 64 bytes) wrapped in ErrTransferRejected.
		return StartTransferOutput{}, fmt.Errorf("%w: %s", ErrTransferRejected, rej.Reason)
	case <-ctx.Done():
		return StartTransferOutput{}, ctx.Err()
	}

	srcPub, ok := tc.cfg.PeerPubKey(in.SourceTrackerID)
	if !ok {
		return StartTransferOutput{}, fmt.Errorf("%w: source pubkey unknown", ErrPeerNotConnected)
	}
	cb, err := fed.CanonicalTransferProofPreSig(proof)
	if err != nil {
		return StartTransferOutput{}, fmt.Errorf("federation: canonical proof: %w", err)
	}
	if !ed25519.Verify(srcPub, cb, proof.SourceTrackerSig) {
		return StartTransferOutput{}, errors.New("federation: source_tracker_sig invalid")
	}

	if err := tc.cfg.Ledger.AppendTransferIn(ctx, TransferInHookIn{
		IdentityID:  idArr,
		Amount:      proof.Amount,
		Timestamp:   proof.Timestamp,
		TransferRef: in.Nonce,
	}); err != nil {
		// ErrTransferRefExists is treated as IDEMPOTENT SUCCESS: a
		// TRANSFER_IN with this ref is already on the local chain, i.e.
		// the credit was booked by an earlier run. This is the
		// dest-restart replay path — the completed cache above was wiped,
		// the source replayed its cached proof (verified against the
		// source pubkey just above), and the ledger's durable on-chain
		// check refused the second credit. Fall through and return the
		// proof-derived result to the caller as if freshly completed.
		if !isLedgerTransferRefExists(err) {
			return StartTransferOutput{}, fmt.Errorf("federation: append transfer_in: %w", err)
		}
		tc.cfg.MetricsCounter("transfer_in_ref_exists_idempotent")
	}

	// Send TransferApplied back to source. Best-effort; failures here are
	// metric+log only since the credit is already booked locally.
	applied := &fed.TransferApplied{
		SourceTrackerId: srcID[:],
		DestTrackerId:   myID[:],
		Nonce:           in.Nonce[:],
		Timestamp:       uint64(tc.cfg.Now().Unix()), //nolint:gosec // G115 — Unix() ≥ 0 for any post-epoch timestamp
	}
	ab, abErr := fed.CanonicalTransferAppliedPreSig(applied)
	if abErr == nil {
		applied.DestTrackerSig = ed25519.Sign(tc.cfg.MyPriv, ab)
		if appliedPayload, mErr := proto.Marshal(applied); mErr == nil {
			_ = tc.cfg.Send(ctx, in.SourceTrackerID, fed.Kind_KIND_TRANSFER_APPLIED, appliedPayload)
		}
	}

	var hashArr [32]byte
	copy(hashArr[:], proof.SourceChainTipHash)
	out := StartTransferOutput{
		SourceChainTipHash: hashArr,
		SourceSeq:          proof.SourceSeq,
		SourceTrackerSig:   proof.SourceTrackerSig,
	}

	// The credit is booked — record the completion so replays of this
	// nonce short-circuit at the top of StartTransfer.
	tc.mu.Lock()
	tc.cacheCompletedLocked(in.Nonce, out)
	tc.mu.Unlock()

	tc.cfg.MetricsCounter("transfer_completed")
	return out, nil
}

// cacheCompletedLocked stores out keyed by nonce, evicting the oldest
// entry if the cap is exceeded. Caller must hold tc.mu. Shares the
// IssuedCap bound with the source-side issued cache.
func (tc *transferCoordinator) cacheCompletedLocked(nonce [32]byte, out StartTransferOutput) {
	if len(tc.completed) >= tc.cfg.IssuedCap {
		var oldestKey [32]byte
		var oldestAt time.Time
		first := true
		for k, v := range tc.completed {
			if first || v.at.Before(oldestAt) {
				oldestKey = k
				oldestAt = v.at
				first = false
			}
		}
		delete(tc.completed, oldestKey)
	}
	tc.completed[nonce] = completedTransfer{out: out, at: tc.cfg.Now()}
}

// OnProof is the dispatcher hook for KIND_TRANSFER_PROOF.
func (tc *transferCoordinator) OnProof(_ context.Context, env *fed.Envelope, fromPeer ids.TrackerID) {
	proof := &fed.TransferProof{}
	if err := proto.Unmarshal(env.Payload, proof); err != nil {
		tc.cfg.MetricsCounter("transfer_proof_shape")
		return
	}
	if err := fed.ValidateTransferProof(proof); err != nil {
		tc.cfg.MetricsCounter("transfer_proof_shape")
		return
	}
	myID := tc.cfg.MyTrackerID.Bytes()
	if !equalID(proof.DestTrackerId, myID[:]) {
		tc.cfg.MetricsCounter("transfer_proof_misrouted")
		return
	}
	srcID := fromPeer.Bytes()
	if !equalID(proof.SourceTrackerId, srcID[:]) {
		tc.cfg.MetricsCounter("transfer_proof_source_mismatch")
		return
	}
	var nonceArr [32]byte
	copy(nonceArr[:], proof.Nonce)

	tc.mu.Lock()
	pending, ok := tc.pending[nonceArr]
	tc.mu.Unlock()
	if !ok {
		tc.cfg.MetricsCounter("transfer_proof_orphan")
		return
	}
	select {
	case pending.ch <- proof:
		tc.cfg.MetricsCounter("transfer_proof_delivered")
	default:
		tc.cfg.MetricsCounter("transfer_proof_buffer_full")
	}
}

// OnApplied is the dispatcher hook for KIND_TRANSFER_APPLIED. The
// destination-side credit is already booked on the source by the time
// this arrives; the message is a confirmation for source-side
// observability.
func (tc *transferCoordinator) OnApplied(_ context.Context, env *fed.Envelope, fromPeer ids.TrackerID) {
	applied := &fed.TransferApplied{}
	if err := proto.Unmarshal(env.Payload, applied); err != nil {
		tc.cfg.MetricsCounter("transfer_applied_shape")
		return
	}
	if err := fed.ValidateTransferApplied(applied); err != nil {
		tc.cfg.MetricsCounter("transfer_applied_shape")
		return
	}
	myID := tc.cfg.MyTrackerID.Bytes()
	if !equalID(applied.SourceTrackerId, myID[:]) {
		tc.cfg.MetricsCounter("transfer_applied_misrouted")
		return
	}
	dstPub, ok := tc.cfg.PeerPubKey(fromPeer)
	if !ok {
		tc.cfg.MetricsCounter("transfer_applied_unknown_peer")
		return
	}
	cb, err := fed.CanonicalTransferAppliedPreSig(applied)
	if err != nil {
		tc.cfg.MetricsCounter("transfer_applied_canonical")
		return
	}
	if !ed25519.Verify(dstPub, cb, applied.DestTrackerSig) {
		tc.cfg.MetricsCounter("transfer_applied_sig")
		return
	}
	tc.cfg.MetricsCounter("transfer_applied_received_ok")
}

// isLedgerTransferRefExists is the federation-internal indirection
// for the ledger's ErrTransferRefExists sentinel. The ledger orchestrator
// package is not imported here to keep federation's dependency surface
// independent of ledger-package identifier renames. The ledger returns
// the sentinel unwrapped (appendLocked's per-kind single-use transfer
// ref check — TRANSFER_OUT at the source, TRANSFER_IN at the
// destination), and the production ledgerHooksAdapter propagates it
// verbatim, so the exact-message match is stable. StartTransfer relies
// on it to turn a dest-restart replay (AppendTransferIn refusing a
// duplicate on-chain TRANSFER_IN ref) into idempotent success.
func isLedgerTransferRefExists(err error) bool {
	if err == nil {
		return false
	}
	const sentinelMsg = "ledger: transfer ref already on chain"
	return err.Error() == sentinelMsg
}

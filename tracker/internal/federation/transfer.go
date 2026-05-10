// Package federation: cross-region credit transfer coordinator.
//
// transferCoordinator owns:
//   - the destination-side pending-request map keyed by Nonce
//   - the source-side issued-proof replay cache keyed by Nonce
//   - the destination-side completed-transfer cache keyed by Nonce
//
// All three are in-memory only in v1. A persistent ledger-indexed
// duplicate check is the §14 follow-up.
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

type pendingResp struct {
	ch chan *fed.TransferProof
}

// transferCoordinator is constructed by Federation.Open and lives for
// the federation subsystem's lifetime.
type transferCoordinator struct {
	cfg transferCoordinatorCfg

	mu      sync.Mutex
	issued  map[[32]byte]issuedProof // source-side replay cache
	pending map[[32]byte]pendingResp // dest-side in-flight
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
		cfg:     cfg,
		issued:  make(map[[32]byte]issuedProof),
		pending: make(map[[32]byte]pendingResp),
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
	out, err := tc.cfg.Ledger.AppendTransferOut(ctx, TransferOutHookIn{
		IdentityID:  identityArr,
		Amount:      req.Amount,
		Timestamp:   req.Timestamp,
		TransferRef: nonceArr,
		ConsumerSig: req.ConsumerSig,
		ConsumerPub: req.ConsumerPub,
	})
	if err != nil {
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
	tc.mu.Lock()
	if _, exists := tc.pending[in.Nonce]; exists {
		tc.mu.Unlock()
		return StartTransferOutput{}, errors.New("federation: duplicate StartTransfer nonce in flight")
	}
	tc.pending[in.Nonce] = pendingResp{ch: ch}
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
		// ErrTransferRefExists is treated as success: the credit is already
		// booked. v1 ledger never returns it, but the contract is in place.
		if !isLedgerTransferRefExists(err) {
			return StartTransferOutput{}, fmt.Errorf("federation: append transfer_in: %w", err)
		}
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
	tc.cfg.MetricsCounter("transfer_completed")
	return StartTransferOutput{
		SourceChainTipHash: hashArr,
		SourceSeq:          proof.SourceSeq,
		SourceTrackerSig:   proof.SourceTrackerSig,
	}, nil
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
// for the ledger's ErrTransferRefExists sentinel. The ledger package
// is not imported here to keep federation's dependency surface
// independent of ledger-package identifier renames; the api wiring
// site can plug a typed predicate if needed. v1 ledger never returns
// the sentinel, so this returns false in production.
func isLedgerTransferRefExists(err error) bool {
	if err == nil {
		return false
	}
	const sentinelMsg = "ledger: transfer ref already on chain"
	return err.Error() == sentinelMsg
}

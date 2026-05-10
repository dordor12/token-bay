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

// transferCoordinator is constructed by Federation.Open and lives for
// the federation subsystem's lifetime. The pending and completed maps
// land in the destination-side StartTransfer follow-up.
type transferCoordinator struct {
	cfg transferCoordinatorCfg

	mu     sync.Mutex
	issued map[[32]byte]issuedProof // source-side replay cache
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
		cfg:    cfg,
		issued: make(map[[32]byte]issuedProof),
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

// keep imports stable across follow-up tasks.
var (
	_ = fmt.Errorf
	_ = errors.New
)

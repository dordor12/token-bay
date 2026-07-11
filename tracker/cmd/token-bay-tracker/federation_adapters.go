package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/api"
	"github.com/token-bay/token-bay/tracker/internal/federation"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
	"github.com/token-bay/token-bay/tracker/internal/ledger/entry"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

// ledgerRootSourceAdapter implements federation.RootSource against the
// existing ledger.Ledger. Returns ok=false until the ledger orchestrator
// produces a root for the requested hour. Wiring the adapter now keeps
// run_cmd.go stable across the orchestrator handoff.
type ledgerRootSourceAdapter struct {
	led *ledger.Ledger
}

func (a ledgerRootSourceAdapter) ReadyRoot(ctx context.Context, hour uint64) ([]byte, []byte, bool, error) {
	root, sig, ok, err := a.led.MerkleRoot(ctx, hour)
	if err != nil || !ok {
		return nil, nil, ok, err
	}
	return root, sig, true, nil
}

// storeAsArchive adapts *storage.Store to federation.PeerRootArchive.
type storeAsArchive struct{ store *storage.Store }

func (s storeAsArchive) PutPeerRoot(ctx context.Context, p storage.PeerRoot) error {
	return s.store.PutPeerRoot(ctx, p)
}

func (s storeAsArchive) GetPeerRoot(ctx context.Context, trackerID []byte, hour uint64) (storage.PeerRoot, bool, error) {
	return s.store.GetPeerRoot(ctx, trackerID, hour)
}

func hexToTrackerID(s string) (ids.TrackerID, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return ids.TrackerID{}, fmt.Errorf("tracker_id must be 32 hex bytes")
	}
	var out ids.TrackerID
	copy(out[:], b)
	return out, nil
}

func hexToPubKey(s string) (ed25519.PublicKey, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("pubkey must be 32 hex bytes")
	}
	return ed25519.PublicKey(b), nil
}

// bootstrapPeersAdapter implements api.BootstrapPeersService against the
// SQLite store + the tracker's identity Ed25519 key. The composition
// root passes the *storage.Store, the tracker pubkey hash (= IssuerID),
// the priv key, and the cfg-derived MaxPeers / TTL.
type bootstrapPeersAdapter struct {
	store    *storage.Store
	issuer   ids.IdentityID
	priv     ed25519.PrivateKey
	maxPeers int
	ttl      time.Duration
}

func (a bootstrapPeersAdapter) ListKnownPeers(ctx context.Context, limit int, byHealthDesc bool) ([]storage.KnownPeer, error) {
	return a.store.ListKnownPeers(ctx, limit, byHealthDesc)
}

func (a bootstrapPeersAdapter) IssuerID() ids.IdentityID { return a.issuer }

func (a bootstrapPeersAdapter) Sign(canonical []byte) ([]byte, error) {
	return ed25519.Sign(a.priv, canonical), nil
}

func (a bootstrapPeersAdapter) MaxPeers() int      { return a.maxPeers }
func (a bootstrapPeersAdapter) TTL() time.Duration { return a.ttl }

// transferFederationAdapter bridges the api package's tbproto-typed
// federationStartTransfer interface to the internal federation
// package's StartTransfer entry point. The api handler at the
// destination tracker has already verified the consumer signature
// against the federation-canonical bytes; this adapter just shovels
// the validated fields into federation.StartTransferInput, fans the
// result back as a tbproto.TransferProof.
type transferFederationAdapter struct {
	fed *federation.Federation
}

func (a transferFederationAdapter) StartTransfer(ctx context.Context, req *tbproto.TransferRequest) (*tbproto.TransferProof, error) {
	if req == nil {
		return nil, fmt.Errorf("transferFederationAdapter: nil request")
	}
	in := federation.StartTransferInput{
		Amount:      req.Amount,
		ConsumerSig: req.ConsumerSig,
		ConsumerPub: ed25519.PublicKey(req.ConsumerPub),
		Timestamp:   req.Timestamp,
	}
	copy(in.SourceTrackerID[:], req.SourceTrackerId)
	copy(in.IdentityID[:], req.IdentityId)
	copy(in.Nonce[:], req.Nonce)

	out, err := a.fed.StartTransfer(ctx, in)
	if err != nil {
		// A source-side rejection (e.g. insufficient balance) carries a
		// human-readable reason (validator-capped to 64 bytes). Surface it
		// as an INVALID RPC error so the consumer sees WHY, instead of the
		// router redacting a generic error to "internal error".
		if errors.Is(err, federation.ErrTransferRejected) {
			return nil, api.ErrInvalid(err.Error())
		}
		return nil, err
	}
	return &tbproto.TransferProof{
		SourceChainTipHash: out.SourceChainTipHash[:],
		SourceSeq:          out.SourceSeq,
		TrackerSig:         out.SourceTrackerSig,
	}, nil
}

// staleTipRetries bounds the ledgerHooksAdapter's ErrStaleTip refresh
// loop. The transfer intent sig is sequencing-independent, so each retry
// re-reads the tip and reuses the SAME consumer sig; the bound only
// guards against a pathologically hot chain starving the transfer.
const staleTipRetries = 8

// ledgerHooksAdapter implements federation.LedgerHooks against the real
// *ledger.Ledger — the production binding for cross-region transfers.
// Each Append* reads the current tip, fills (prev_hash, seq), and retries
// on ErrStaleTip. The transfer_out commits durably BEFORE the federation
// layer signs and returns the TransferProof (ordering invariant: no proof
// without an on-chain debit). The ledger's on-chain single-use
// per-kind transfer ref checks (ledger.ErrTransferRefExists) are the
// durable backstops — TRANSFER_OUT against double-debit at the source,
// TRANSFER_IN against double-credit at the destination (dest-restart
// replay); the adapter propagates the sentinel verbatim so federation's
// isLedgerTransferRefExists can classify it.
type ledgerHooksAdapter struct {
	led *ledger.Ledger
}

func (a ledgerHooksAdapter) nextTip(ctx context.Context) (prev []byte, seq uint64, err error) {
	tipSeq, tipHash, hasTip, err := a.led.Tip(ctx)
	if err != nil {
		return nil, 0, err
	}
	if !hasTip {
		return make([]byte, 32), 1, nil
	}
	return tipHash, tipSeq + 1, nil
}

// AppendTransferOut maps the federation hook input onto a
// ledger.TransferOutRecord. Every field flows through — including
// SourceTrackerID/DestTrackerID, which the ledger needs to reconstruct
// the exact canonical transfer-proof-request preimage the consumer
// signed; the Timestamp doubles as the EntryBody timestamp and the
// signed request timestamp.
func (a ledgerHooksAdapter) AppendTransferOut(ctx context.Context, in federation.TransferOutHookIn) (federation.TransferOutHookOut, error) {
	for i := 0; i < staleTipRetries; i++ {
		prev, seq, err := a.nextTip(ctx)
		if err != nil {
			return federation.TransferOutHookOut{}, err
		}
		e, err := a.led.AppendTransferOut(ctx, ledger.TransferOutRecord{
			PrevHash:        prev,
			Seq:             seq,
			ConsumerID:      in.IdentityID[:],
			Amount:          in.Amount,
			Timestamp:       in.Timestamp,
			TransferRef:     in.TransferRef[:],
			SourceTrackerID: in.SourceTrackerID[:],
			DestTrackerID:   in.DestTrackerID[:],
			ConsumerSig:     in.ConsumerSig,
			ConsumerPub:     in.ConsumerPub,
		})
		if errors.Is(err, ledger.ErrStaleTip) {
			continue
		}
		if err != nil {
			return federation.TransferOutHookOut{}, err
		}
		h, err := entry.Hash(e.Body)
		if err != nil {
			return federation.TransferOutHookOut{}, err
		}
		return federation.TransferOutHookOut{ChainTipHash: h, Seq: e.Body.Seq}, nil
	}
	return federation.TransferOutHookOut{}, ledger.ErrStaleTip
}

func (a ledgerHooksAdapter) AppendTransferIn(ctx context.Context, in federation.TransferInHookIn) error {
	for i := 0; i < staleTipRetries; i++ {
		prev, seq, err := a.nextTip(ctx)
		if err != nil {
			return err
		}
		_, err = a.led.AppendTransferIn(ctx, ledger.TransferInRecord{
			PrevHash:    prev,
			Seq:         seq,
			IdentityID:  in.IdentityID[:],
			Amount:      in.Amount,
			Timestamp:   in.Timestamp,
			TransferRef: in.TransferRef[:],
		})
		if errors.Is(err, ledger.ErrStaleTip) {
			continue
		}
		return err
	}
	return ledger.ErrStaleTip
}

// silence unused warnings if any helper goes briefly unused.
var (
	_ federation.RootSource      = ledgerRootSourceAdapter{}
	_ federation.PeerRootArchive = storeAsArchive{}
	_ api.BootstrapPeersService  = bootstrapPeersAdapter{}
	_ api.FederationService      = transferFederationAdapter{}
	_ federation.LedgerHooks     = ledgerHooksAdapter{}
)

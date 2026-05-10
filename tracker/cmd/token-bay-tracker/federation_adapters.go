package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/federation"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
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

// silence unused warnings if any helper goes briefly unused.
var (
	_ federation.RootSource      = ledgerRootSourceAdapter{}
	_ federation.PeerRootArchive = storeAsArchive{}
)

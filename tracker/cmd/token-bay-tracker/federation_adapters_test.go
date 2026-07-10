package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/federation"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
	"github.com/token-bay/token-bay/tracker/internal/ledger/entry"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

// e2eKeypair derives a deterministic Ed25519 keypair from a label.
func e2eKeypair(label string) (ed25519.PublicKey, ed25519.PrivateKey) {
	seed := sha256.Sum256([]byte("cmd-e2e-" + label))
	priv := ed25519.NewKeyFromSeed(seed[:])
	return priv.Public().(ed25519.PublicKey), priv
}

// openE2ELedger opens a real SQLite-backed ledger in a tempdir, signed by
// the given tracker key — the same wiring run_cmd.go performs at startup.
func openE2ELedger(t *testing.T, name string, key ed25519.PrivateKey) (*ledger.Ledger, *storage.Store) {
	t.Helper()
	store, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), name+".db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	led, err := ledger.Open(store, key)
	require.NoError(t, err)
	t.Cleanup(func() { _ = led.Close() })
	return led, store
}

// TestTransfer_RealLedgerAdapter_EndToEnd is the end-to-end preimage-
// equality proof for cross-region transfer: TWO federation.Open instances
// over the in-proc transport hub, BOTH with Deps.Ledger set to the
// production ledgerHooksAdapter over separate REAL SQLite ledgers. The
// consumer signs the canonical TransferProofRequest intent once; the
// source ledger must reconstruct the exact same preimage from the hook
// input (including both tracker ids) or the transfer fails.
//
// Asserts: transfer_out on the source chain, transfer_in on the dest
// chain, balances move by the amount at both ends, a valid TransferProof
// is returned, and a replayed StartTransfer with the same nonce is
// idempotent (no double-credit, no double-debit).
func TestTransfer_RealLedgerAdapter_EndToEnd(t *testing.T) {
	ctx := context.Background()

	aPub, aPriv := e2eKeypair("source-tracker") // A = source (holds the credits)
	bPub, bPriv := e2eKeypair("dest-tracker")   // B = destination (asks for the proof)
	aID := ids.TrackerID(sha256.Sum256(aPub))
	bID := ids.TrackerID(sha256.Sum256(bPub))

	ledA, storeA := openE2ELedger(t, "source", aPriv)
	ledB, storeB := openE2ELedger(t, "dest", bPriv)

	// The consumer identity is the SPKI-hash of its pubkey — the same
	// encoding the enrollment path derives, and the binding
	// AppendTransferOut enforces.
	conPub, conPriv := e2eKeypair("consumer")
	spki, err := x509.MarshalPKIXPublicKey(conPub)
	require.NoError(t, err)
	identity := sha256.Sum256(spki)

	// Fund the consumer at the SOURCE.
	_, err = ledA.IssueStarterGrant(ctx, identity[:], 5000)
	require.NoError(t, err)

	hub := federation.NewInprocHub()
	trA := federation.NewInprocTransport(hub, "A", aPub, aPriv)
	trB := federation.NewInprocTransport(hub, "B", bPub, bPriv)

	aFed, err := federation.Open(federation.Config{
		MyTrackerID: aID,
		MyPriv:      aPriv,
		Peers:       []federation.AllowlistedPeer{{TrackerID: bID, PubKey: bPub, Addr: "B"}},
	}, federation.Deps{
		Transport: trA,
		RootSrc:   ledgerRootSourceAdapter{led: ledA},
		Archive:   storeAsArchive{store: storeA},
		Ledger:    ledgerHooksAdapter{led: ledA},
		Metrics:   federation.NewMetrics(prometheus.NewRegistry()),
		Logger:    zerolog.Nop(),
		Now:       time.Now,
	})
	require.NoError(t, err)
	bFed, err := federation.Open(federation.Config{
		MyTrackerID: bID,
		MyPriv:      bPriv,
		Peers:       []federation.AllowlistedPeer{{TrackerID: aID, PubKey: aPub, Addr: "A"}},
	}, federation.Deps{
		Transport: trB,
		RootSrc:   ledgerRootSourceAdapter{led: ledB},
		Archive:   storeAsArchive{store: storeB},
		Ledger:    ledgerHooksAdapter{led: ledB},
		Metrics:   federation.NewMetrics(prometheus.NewRegistry()),
		Logger:    zerolog.Nop(),
		Now:       time.Now,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = aFed.Close()
		_ = bFed.Close()
	})

	// Wait for B → A peering to reach steady state.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		steady := false
		for _, p := range bFed.Peers() {
			if p.State == federation.PeerStateSteady {
				steady = true
				break
			}
		}
		if steady {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The consumer signs the canonical transfer intent ONCE. These exact
	// bytes must re-verify inside the source ledger's AppendTransferOut.
	var nonce [32]byte
	for i := range nonce {
		nonce[i] = 0x5A
	}
	aIDBytes := aID.Bytes()
	bIDBytes := bID.Bytes()
	req := &fed.TransferProofRequest{
		SourceTrackerId: aIDBytes[:],
		DestTrackerId:   bIDBytes[:],
		IdentityId:      identity[:],
		Amount:          1500,
		Nonce:           nonce[:],
		ConsumerPub:     conPub,
		Timestamp:       1714000000,
	}
	sig, err := fed.SignTransferProofRequest(conPriv, req)
	require.NoError(t, err)

	in := federation.StartTransferInput{
		SourceTrackerID: aID,
		IdentityID:      ids.IdentityID(identity),
		Amount:          1500,
		Nonce:           nonce,
		ConsumerSig:     sig,
		ConsumerPub:     conPub,
		Timestamp:       1714000000,
	}
	out, err := bFed.StartTransfer(ctx, in)
	require.NoError(t, err, "real two-tracker transfer must succeed end-to-end")
	require.NotEmpty(t, out.SourceTrackerSig, "TransferProof must carry the source tracker sig")

	// SOURCE chain: seq 1 = starter grant, seq 2 = transfer_out.
	eOut, ok, err := ledA.EntryBySeq(ctx, 2)
	require.NoError(t, err)
	require.True(t, ok, "transfer_out must be on the source chain")
	assert.Equal(t, tbproto.EntryKind_ENTRY_KIND_TRANSFER_OUT, eOut.Body.Kind)
	assert.Equal(t, nonce[:], eOut.Body.Ref)
	assert.Equal(t, uint64(1500), eOut.Body.CostCredits)
	assert.Equal(t, sig, eOut.ConsumerSig, "intent-domain consumer sig stored verbatim")

	// The proof's chain-tip hash and seq must match the committed entry.
	wantHash, err := entry.Hash(eOut.Body)
	require.NoError(t, err)
	assert.Equal(t, wantHash, out.SourceChainTipHash)
	assert.Equal(t, uint64(2), out.SourceSeq)

	// SOURCE balance debited by exactly the amount.
	snapA, err := ledA.SignedBalance(ctx, identity[:])
	require.NoError(t, err)
	assert.Equal(t, int64(3500), snapA.Body.Credits, "source debited 5000-1500")

	// DEST chain: seq 1 = transfer_in with the same ref; balance credited.
	eIn, ok, err := ledB.EntryBySeq(ctx, 1)
	require.NoError(t, err)
	require.True(t, ok, "transfer_in must be on the dest chain")
	assert.Equal(t, tbproto.EntryKind_ENTRY_KIND_TRANSFER_IN, eIn.Body.Kind)
	assert.Equal(t, nonce[:], eIn.Body.Ref)
	assert.Equal(t, uint64(1500), eIn.Body.CostCredits)

	snapB, err := ledB.SignedBalance(ctx, identity[:])
	require.NoError(t, err)
	assert.Equal(t, int64(1500), snapB.Body.Credits, "dest credited the amount")

	// Replay the same nonce end-to-end: the dest completed cache returns
	// the cached proof; neither ledger moves.
	replay, err := bFed.StartTransfer(ctx, in)
	require.NoError(t, err, "replayed transfer must be idempotent")
	assert.Equal(t, out, replay)

	tipA, _, _, err := ledA.Tip(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), tipA, "source chain unchanged on replay")
	tipB, _, _, err := ledB.Tip(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), tipB, "dest chain unchanged on replay")

	snapA2, err := ledA.SignedBalance(ctx, identity[:])
	require.NoError(t, err)
	assert.Equal(t, int64(3500), snapA2.Body.Credits, "NO double-debit at source")
	snapB2, err := ledB.SignedBalance(ctx, identity[:])
	require.NoError(t, err)
	assert.Equal(t, int64(1500), snapB2.Body.Credits, "NO double-credit at dest")
}

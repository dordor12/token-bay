package federation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
)

// fakeLedger implements LedgerHooks for transfer-coordinator unit tests.
type fakeLedger struct {
	mu sync.Mutex

	outCalls []TransferOutHookIn
	inCalls  []TransferInHookIn

	outResult TransferOutHookOut
	outErr    error
	inErr     error
}

func (f *fakeLedger) AppendTransferOut(_ context.Context, in TransferOutHookIn) (TransferOutHookOut, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outCalls = append(f.outCalls, in)
	return f.outResult, f.outErr
}

func (f *fakeLedger) AppendTransferIn(_ context.Context, in TransferInHookIn) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inCalls = append(f.inCalls, in)
	return f.inErr
}

func keypairFromSeed(seed byte) (ed25519.PublicKey, ed25519.PrivateKey) {
	s := make([]byte, ed25519.SeedSize)
	for i := range s {
		s[i] = seed
	}
	priv := ed25519.NewKeyFromSeed(s)
	return priv.Public().(ed25519.PublicKey), priv
}

func bytes32(b byte) [32]byte {
	var out [32]byte
	for i := range out {
		out[i] = b
	}
	return out
}

func trackerID(pub ed25519.PublicKey) ids.TrackerID {
	return ids.TrackerID(sha256.Sum256(pub))
}

func TestTransferCoordinator_OnRequest_HappyPath(t *testing.T) {
	t.Parallel()
	srcPub, srcPriv := keypairFromSeed(0x11)
	dstPub, dstPriv := keypairFromSeed(0x22)
	conPub, conPriv := keypairFromSeed(0x33)

	srcID := trackerID(srcPub)
	dstID := trackerID(dstPub)

	ledger := &fakeLedger{
		outResult: TransferOutHookOut{
			ChainTipHash: sha256.Sum256([]byte("chain-tip")),
			Seq:          42,
		},
	}

	var sentPayloads [][]byte
	var sentMu sync.Mutex
	send := func(_ context.Context, peerID ids.TrackerID, kind fed.Kind, payload []byte) error {
		sentMu.Lock()
		defer sentMu.Unlock()
		assert.Equal(t, dstID, peerID)
		assert.Equal(t, fed.Kind_KIND_TRANSFER_PROOF, kind)
		sentPayloads = append(sentPayloads, payload)
		return nil
	}

	tc := newTransferCoordinator(transferCoordinatorCfg{
		MyTrackerID: srcID,
		MyPriv:      srcPriv,
		Ledger:      ledger,
		IssuedCap:   16,
		Now:         func() time.Time { return time.Unix(1714000000, 0) },
		PeerPubKey:  func(id ids.TrackerID) (ed25519.PublicKey, bool) { return dstPub, id == dstID },
		Send:        send,
	})

	identityID := bytes32(0x44)
	nonce := bytes32(0x55)

	srcIDBytes := srcID.Bytes()
	dstIDBytes := dstID.Bytes()
	req := &fed.TransferProofRequest{
		SourceTrackerId: srcIDBytes[:],
		DestTrackerId:   dstIDBytes[:],
		IdentityId:      identityID[:],
		Amount:          1500,
		Nonce:           nonce[:],
		ConsumerPub:     conPub,
		Timestamp:       1714000000,
	}
	canonical, err := fed.CanonicalTransferProofRequestPreSig(req)
	require.NoError(t, err)
	req.ConsumerSig = ed25519.Sign(conPriv, canonical)

	payload, err := proto.Marshal(req)
	require.NoError(t, err)
	env, err := SignEnvelope(dstPriv, dstIDBytes[:], fed.Kind_KIND_TRANSFER_PROOF_REQUEST, payload)
	require.NoError(t, err)

	tc.OnRequest(context.Background(), env, dstID)

	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	require.Len(t, ledger.outCalls, 1)
	got := ledger.outCalls[0]
	assert.Equal(t, identityID, got.IdentityID)
	assert.Equal(t, uint64(1500), got.Amount)
	assert.Equal(t, nonce, got.TransferRef)
	assert.Equal(t, uint64(1714000000), got.Timestamp)
	assert.Equal(t, []byte(conPub), []byte(got.ConsumerPub))
	assert.Equal(t, req.ConsumerSig, got.ConsumerSig)

	sentMu.Lock()
	defer sentMu.Unlock()
	require.Len(t, sentPayloads, 1)
	proof := &fed.TransferProof{}
	require.NoError(t, proto.Unmarshal(sentPayloads[0], proof))
	assert.Equal(t, uint64(42), proof.SourceSeq)
	assert.Equal(t, uint64(1500), proof.Amount)
	assert.True(t, bytes.Equal(srcIDBytes[:], proof.SourceTrackerId))
	assert.True(t, bytes.Equal(dstIDBytes[:], proof.DestTrackerId))
	assert.NotEmpty(t, proof.SourceTrackerSig)
}

func TestTransferCoordinator_OnRequest_BadConsumerSig(t *testing.T) {
	t.Parallel()
	srcPub, srcPriv := keypairFromSeed(0x11)
	dstPub, dstPriv := keypairFromSeed(0x22)
	conPub, _ := keypairFromSeed(0x33)
	_, attackerPriv := keypairFromSeed(0x77)

	srcID := trackerID(srcPub)
	dstID := trackerID(dstPub)

	ledger := &fakeLedger{}
	metrics := map[string]int{}
	var metricsMu sync.Mutex
	tc := newTransferCoordinator(transferCoordinatorCfg{
		MyTrackerID: srcID,
		MyPriv:      srcPriv,
		Ledger:      ledger,
		Now:         func() time.Time { return time.Unix(1714000000, 0) },
		PeerPubKey:  func(id ids.TrackerID) (ed25519.PublicKey, bool) { return dstPub, id == dstID },
		Send:        func(_ context.Context, _ ids.TrackerID, _ fed.Kind, _ []byte) error { return nil },
		MetricsCounter: func(n string) {
			metricsMu.Lock()
			defer metricsMu.Unlock()
			metrics[n]++
		},
	})

	identityID := bytes32(0x44)
	nonce := bytes32(0x55)

	srcIDBytes := srcID.Bytes()
	dstIDBytes := dstID.Bytes()
	req := &fed.TransferProofRequest{
		SourceTrackerId: srcIDBytes[:],
		DestTrackerId:   dstIDBytes[:],
		IdentityId:      identityID[:],
		Amount:          1500,
		Nonce:           nonce[:],
		ConsumerPub:     conPub,
		Timestamp:       1714000000,
	}
	canonical, err := fed.CanonicalTransferProofRequestPreSig(req)
	require.NoError(t, err)
	req.ConsumerSig = ed25519.Sign(attackerPriv, canonical) // wrong key

	payload, err := proto.Marshal(req)
	require.NoError(t, err)
	env, err := SignEnvelope(dstPriv, dstIDBytes[:], fed.Kind_KIND_TRANSFER_PROOF_REQUEST, payload)
	require.NoError(t, err)

	tc.OnRequest(context.Background(), env, dstID)

	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	assert.Empty(t, ledger.outCalls, "ledger must not be called on bad sig")
	metricsMu.Lock()
	defer metricsMu.Unlock()
	assert.Equal(t, 1, metrics["transfer_request_consumer_sig"])
}

func TestTransferCoordinator_OnRequest_DuplicateNonceReplaysProof(t *testing.T) {
	t.Parallel()
	srcPub, srcPriv := keypairFromSeed(0x11)
	dstPub, dstPriv := keypairFromSeed(0x22)
	conPub, conPriv := keypairFromSeed(0x33)

	srcID := trackerID(srcPub)
	dstID := trackerID(dstPub)

	ledger := &fakeLedger{
		outResult: TransferOutHookOut{
			ChainTipHash: bytes32(0xCC),
			Seq:          7,
		},
	}
	var sentPayloads [][]byte
	var sentMu sync.Mutex
	tc := newTransferCoordinator(transferCoordinatorCfg{
		MyTrackerID: srcID,
		MyPriv:      srcPriv,
		Ledger:      ledger,
		IssuedCap:   16,
		Now:         func() time.Time { return time.Unix(1714000000, 0) },
		PeerPubKey:  func(id ids.TrackerID) (ed25519.PublicKey, bool) { return dstPub, id == dstID },
		Send: func(_ context.Context, _ ids.TrackerID, _ fed.Kind, payload []byte) error {
			sentMu.Lock()
			defer sentMu.Unlock()
			sentPayloads = append(sentPayloads, append([]byte(nil), payload...))
			return nil
		},
	})

	identityID := bytes32(0x44)
	nonce := bytes32(0x55)
	srcIDBytes := srcID.Bytes()
	dstIDBytes := dstID.Bytes()

	makeEnv := func() *fed.Envelope {
		req := &fed.TransferProofRequest{
			SourceTrackerId: srcIDBytes[:],
			DestTrackerId:   dstIDBytes[:],
			IdentityId:      identityID[:],
			Amount:          1500,
			Nonce:           nonce[:],
			ConsumerPub:     conPub,
			Timestamp:       1714000000,
		}
		canonical, err := fed.CanonicalTransferProofRequestPreSig(req)
		require.NoError(t, err)
		req.ConsumerSig = ed25519.Sign(conPriv, canonical)
		payload, err := proto.Marshal(req)
		require.NoError(t, err)
		env, err := SignEnvelope(dstPriv, dstIDBytes[:], fed.Kind_KIND_TRANSFER_PROOF_REQUEST, payload)
		require.NoError(t, err)
		return env
	}

	tc.OnRequest(context.Background(), makeEnv(), dstID)
	ledger.mu.Lock()
	require.Len(t, ledger.outCalls, 1, "first request: ledger called once")
	ledger.mu.Unlock()
	sentMu.Lock()
	require.Len(t, sentPayloads, 1)
	sentMu.Unlock()

	tc.OnRequest(context.Background(), makeEnv(), dstID)
	ledger.mu.Lock()
	assert.Len(t, ledger.outCalls, 1, "second request must be replayed without re-debiting")
	ledger.mu.Unlock()
	sentMu.Lock()
	defer sentMu.Unlock()
	require.Len(t, sentPayloads, 2)
	assert.Equal(t, sentPayloads[0], sentPayloads[1], "replayed proof bytes must equal first proof")
}

func TestTransferCoordinator_StartTransfer_HappyPath(t *testing.T) {
	t.Parallel()
	srcPub, srcPriv := keypairFromSeed(0x11)
	dstPub, dstPriv := keypairFromSeed(0x22)
	conPub, conPriv := keypairFromSeed(0x33)

	srcID := trackerID(srcPub)
	dstID := trackerID(dstPub)

	ledger := &fakeLedger{}

	type sent struct {
		Peer    ids.TrackerID
		Kind    fed.Kind
		Payload []byte
	}
	sendCh := make(chan sent, 4)
	send := func(_ context.Context, peer ids.TrackerID, kind fed.Kind, payload []byte) error {
		sendCh <- sent{peer, kind, append([]byte(nil), payload...)}
		return nil
	}

	tc := newTransferCoordinator(transferCoordinatorCfg{
		MyTrackerID: dstID,
		MyPriv:      dstPriv,
		Ledger:      ledger,
		IssuedCap:   16,
		Now:         func() time.Time { return time.Unix(1714000000, 0) },
		PeerPubKey: func(id ids.TrackerID) (ed25519.PublicKey, bool) {
			if id == srcID {
				return srcPub, true
			}
			return nil, false
		},
		Send: send,
	})

	identityID := bytes32(0x44)
	nonce := bytes32(0x55)

	in := StartTransferInput{
		SourceTrackerID: srcID,
		IdentityID:      ids.IdentityID(identityID),
		Amount:          1500,
		Nonce:           nonce,
		ConsumerPub:     conPub,
		Timestamp:       1714000000,
	}

	srcIDBytes := srcID.Bytes()
	dstIDBytes := dstID.Bytes()
	canonReq := &fed.TransferProofRequest{
		SourceTrackerId: srcIDBytes[:],
		DestTrackerId:   dstIDBytes[:],
		IdentityId:      identityID[:],
		Amount:          1500,
		Nonce:           nonce[:],
		ConsumerPub:     conPub,
		Timestamp:       1714000000,
	}
	canonical, err := fed.CanonicalTransferProofRequestPreSig(canonReq)
	require.NoError(t, err)
	in.ConsumerSig = ed25519.Sign(conPriv, canonical)

	type result struct {
		out StartTransferOutput
		err error
	}
	resultCh := make(chan result, 1)
	go func() {
		out, err := tc.StartTransfer(context.Background(), in)
		resultCh <- result{out, err}
	}()

	var requestSent sent
	select {
	case requestSent = <-sendCh:
	case <-time.After(2 * time.Second):
		t.Fatal("StartTransfer did not Send within 2s")
	}
	assert.Equal(t, srcID, requestSent.Peer)
	assert.Equal(t, fed.Kind_KIND_TRANSFER_PROOF_REQUEST, requestSent.Kind)

	proof := &fed.TransferProof{
		SourceTrackerId:    srcIDBytes[:],
		DestTrackerId:      dstIDBytes[:],
		IdentityId:         identityID[:],
		Amount:             1500,
		Nonce:              nonce[:],
		SourceChainTipHash: bytes.Repeat([]byte{0xCC}, 32),
		SourceSeq:          7,
		Timestamp:          1714000000,
	}
	cb, err := fed.CanonicalTransferProofPreSig(proof)
	require.NoError(t, err)
	proof.SourceTrackerSig = ed25519.Sign(srcPriv, cb)
	payload, err := proto.Marshal(proof)
	require.NoError(t, err)
	env, err := SignEnvelope(srcPriv, srcIDBytes[:], fed.Kind_KIND_TRANSFER_PROOF, payload)
	require.NoError(t, err)
	tc.OnProof(context.Background(), env, srcID)

	select {
	case r := <-resultCh:
		require.NoError(t, r.err)
		assert.Equal(t, uint64(7), r.out.SourceSeq)
		assert.Equal(t, []byte(proof.SourceTrackerSig), r.out.SourceTrackerSig)
		var hashArr [32]byte
		copy(hashArr[:], proof.SourceChainTipHash)
		assert.Equal(t, hashArr, r.out.SourceChainTipHash)
	case <-time.After(2 * time.Second):
		t.Fatal("StartTransfer did not return within 2s")
	}

	ledger.mu.Lock()
	require.Len(t, ledger.inCalls, 1)
	assert.Equal(t, identityID, ledger.inCalls[0].IdentityID)
	assert.Equal(t, uint64(1500), ledger.inCalls[0].Amount)
	assert.Equal(t, nonce, ledger.inCalls[0].TransferRef)
	ledger.mu.Unlock()

	select {
	case applied := <-sendCh:
		assert.Equal(t, srcID, applied.Peer)
		assert.Equal(t, fed.Kind_KIND_TRANSFER_APPLIED, applied.Kind)
	case <-time.After(2 * time.Second):
		t.Fatal("TransferApplied not sent within 2s")
	}
}

func TestTransferCoordinator_OnProof_Orphan(t *testing.T) {
	t.Parallel()
	srcPub, srcPriv := keypairFromSeed(0x11)
	dstPub, dstPriv := keypairFromSeed(0x22)
	srcID := trackerID(srcPub)
	dstID := trackerID(dstPub)

	metrics := map[string]int{}
	var mu sync.Mutex
	tc := newTransferCoordinator(transferCoordinatorCfg{
		MyTrackerID: dstID,
		MyPriv:      dstPriv,
		Ledger:      &fakeLedger{},
		Now:         func() time.Time { return time.Unix(1714000000, 0) },
		PeerPubKey:  func(id ids.TrackerID) (ed25519.PublicKey, bool) { return srcPub, id == srcID },
		Send:        func(context.Context, ids.TrackerID, fed.Kind, []byte) error { return nil },
		MetricsCounter: func(n string) {
			mu.Lock()
			defer mu.Unlock()
			metrics[n]++
		},
	})

	srcIDBytes := srcID.Bytes()
	dstIDBytes := dstID.Bytes()
	proof := &fed.TransferProof{
		SourceTrackerId:    srcIDBytes[:],
		DestTrackerId:      dstIDBytes[:],
		IdentityId:         bytes.Repeat([]byte{0x44}, 32),
		Amount:             1500,
		Nonce:              bytes.Repeat([]byte{0x55}, 32),
		SourceChainTipHash: bytes.Repeat([]byte{0xCC}, 32),
		SourceSeq:          7,
		Timestamp:          1714000000,
	}
	cb, err := fed.CanonicalTransferProofPreSig(proof)
	require.NoError(t, err)
	proof.SourceTrackerSig = ed25519.Sign(srcPriv, cb)
	payload, err := proto.Marshal(proof)
	require.NoError(t, err)
	env, err := SignEnvelope(srcPriv, srcIDBytes[:], fed.Kind_KIND_TRANSFER_PROOF, payload)
	require.NoError(t, err)

	tc.OnProof(context.Background(), env, srcID)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, metrics["transfer_proof_orphan"])
}

func TestTransferCoordinator_OnApplied_HappyPath(t *testing.T) {
	t.Parallel()
	srcPub, srcPriv := keypairFromSeed(0x11)
	dstPub, dstPriv := keypairFromSeed(0x22)
	srcID := trackerID(srcPub)
	dstID := trackerID(dstPub)

	metrics := map[string]int{}
	var mu sync.Mutex
	tc := newTransferCoordinator(transferCoordinatorCfg{
		MyTrackerID: srcID,
		MyPriv:      srcPriv,
		Ledger:      &fakeLedger{},
		Now:         func() time.Time { return time.Unix(1714000000, 0) },
		PeerPubKey: func(id ids.TrackerID) (ed25519.PublicKey, bool) {
			if id == dstID {
				return dstPub, true
			}
			return nil, false
		},
		Send: func(context.Context, ids.TrackerID, fed.Kind, []byte) error { return nil },
		MetricsCounter: func(n string) {
			mu.Lock()
			defer mu.Unlock()
			metrics[n]++
		},
	})

	srcIDBytes := srcID.Bytes()
	dstIDBytes := dstID.Bytes()
	applied := &fed.TransferApplied{
		SourceTrackerId: srcIDBytes[:],
		DestTrackerId:   dstIDBytes[:],
		Nonce:           bytes.Repeat([]byte{0x55}, 32),
		Timestamp:       1714000001,
	}
	cb, err := fed.CanonicalTransferAppliedPreSig(applied)
	require.NoError(t, err)
	applied.DestTrackerSig = ed25519.Sign(dstPriv, cb)
	payload, err := proto.Marshal(applied)
	require.NoError(t, err)
	env, err := SignEnvelope(dstPriv, dstIDBytes[:], fed.Kind_KIND_TRANSFER_APPLIED, payload)
	require.NoError(t, err)

	tc.OnApplied(context.Background(), env, dstID)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, metrics["transfer_applied_received_ok"])
}

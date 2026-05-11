package federation_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/api"
	"github.com/token-bay/token-bay/tracker/internal/federation"
)

// apiTransferAdapter is the test-local equivalent of the adapter wired
// up in cmd/token-bay-tracker/run_cmd.go. It satisfies
// api.FederationService by translating tbproto.TransferRequest into
// federation.StartTransferInput, calling the destination federation's
// StartTransfer, and packing the result into tbproto.TransferProof.
type apiTransferAdapter struct {
	fed *federation.Federation
}

func (a apiTransferAdapter) StartTransfer(ctx context.Context, req *tbproto.TransferRequest) (*tbproto.TransferProof, error) {
	if req == nil {
		return nil, fmt.Errorf("apiTransferAdapter: nil request")
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
		return nil, err
	}
	return &tbproto.TransferProof{
		SourceChainTipHash: out.SourceChainTipHash[:],
		SourceSeq:          out.SourceSeq,
		TrackerSig:         out.SourceTrackerSig,
	}, nil
}

// TestIntegration_TransferRequest_APIToFederation_EndToEnd drives the
// full cross-region transfer through Router.Dispatch on the destination
// tracker. Federation B is the destination; A is the source, holding
// the consumer's credits. After Dispatch returns:
//   - A's fakeIntegrationLedger records exactly one AppendTransferOut
//     with the right amount + nonce.
//   - B's fakeIntegrationLedger eventually records one AppendTransferIn
//     for the same consumer.
//   - The Router response carries B's signed TransferProof bytes.
func TestIntegration_TransferRequest_APIToFederation_EndToEnd(t *testing.T) {
	t.Parallel()

	// Build a two-tracker federation pair locally so we retain access
	// to both raw pubkeys — needed to wire the api router's TrackerPub
	// at the destination tracker B.
	aPub, aPriv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	bPub, bPriv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	aID := ids.TrackerID(sha256.Sum256(aPub))
	bID := ids.TrackerID(sha256.Sum256(bPub))

	hub := federation.NewInprocHub()
	trA := federation.NewInprocTransport(hub, "A", aPub, aPriv)
	trB := federation.NewInprocTransport(hub, "B", bPub, bPriv)

	aLedger := &fakeIntegrationLedger{
		outResult: federation.TransferOutHookOut{
			ChainTipHash: [32]byte{
				0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB,
				0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB,
				0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB,
				0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB,
			},
			Seq: 42,
		},
	}
	bLedger := &fakeIntegrationLedger{}

	srcRootA := &fakeRootSrc{ok: false}
	srcRootB := &fakeRootSrc{ok: false}
	archA, archB := newFakeArchive(), newFakeArchive()

	aFed, err := federation.Open(federation.Config{
		MyTrackerID: aID,
		MyPriv:      aPriv,
		Peers:       []federation.AllowlistedPeer{{TrackerID: bID, PubKey: bPub, Addr: "B"}},
	}, federation.Deps{
		Transport: trA, RootSrc: srcRootA, Archive: archA, Ledger: aLedger,
		Metrics: federation.NewMetrics(prometheus.NewRegistry()),
		Logger:  zerolog.Nop(), Now: time.Now,
	})
	require.NoError(t, err)
	bFed, err := federation.Open(federation.Config{
		MyTrackerID: bID,
		MyPriv:      bPriv,
		Peers:       []federation.AllowlistedPeer{{TrackerID: aID, PubKey: aPub, Addr: "A"}},
	}, federation.Deps{
		Transport: trB, RootSrc: srcRootB, Archive: archB, Ledger: bLedger,
		Metrics: federation.NewMetrics(prometheus.NewRegistry()),
		Logger:  zerolog.Nop(), Now: time.Now,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = aFed.Close()
		_ = bFed.Close()
	})

	// Wait for B → A peering steady before issuing the transfer.
	deadline := time.Now().Add(2 * time.Second)
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

	router, err := api.NewRouter(api.Deps{
		Logger:     zerolog.Nop(),
		Now:        time.Now,
		Federation: apiTransferAdapter{fed: bFed},
		TrackerPub: bPub,
	})
	require.NoError(t, err)

	conPub, conPriv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	var identity [32]byte
	identity[0] = 0x44
	var nonce [32]byte
	for i := range nonce {
		nonce[i] = 0x55
	}
	aIDBytes := aID.Bytes()
	bIDBytes := bID.Bytes()

	fedReq := &fed.TransferProofRequest{
		SourceTrackerId: aIDBytes[:],
		DestTrackerId:   bIDBytes[:],
		IdentityId:      identity[:],
		Amount:          1500,
		Nonce:           nonce[:],
		ConsumerPub:     conPub,
		Timestamp:       1714000000,
	}
	sig, err := fed.SignTransferProofRequest(conPriv, fedReq)
	require.NoError(t, err)

	rpcReq := &tbproto.TransferRequest{
		IdentityId:      identity[:],
		Amount:          1500,
		DestRegion:      "destination-region",
		Nonce:           nonce[:],
		SourceTrackerId: aIDBytes[:],
		ConsumerSig:     sig,
		ConsumerPub:     conPub,
		Timestamp:       1714000000,
	}
	payload, err := proto.Marshal(rpcReq)
	require.NoError(t, err)

	rc := &api.RequestCtx{
		PeerID:     ids.IdentityID{0x77},
		RemoteAddr: netip.MustParseAddrPort("127.0.0.1:55001"),
		Now:        time.Unix(1714000000, 0),
		Logger:     zerolog.Nop(),
	}
	resp := router.Dispatch(context.Background(), rc, &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_TRANSFER_REQUEST,
		Payload: payload,
	})
	require.Nil(t, resp.Error, "Dispatch returned error: %+v", resp.Error)
	require.Equal(t, tbproto.RpcStatus_RPC_STATUS_OK, resp.Status)

	var proof tbproto.TransferProof
	require.NoError(t, proto.Unmarshal(resp.Payload, &proof))
	assert.Equal(t, uint64(42), proof.SourceSeq)

	require.Len(t, aLedger.snapshotOutCalls(), 1, "A must have appended exactly one transfer_out")
	outCall := aLedger.snapshotOutCalls()[0]
	assert.Equal(t, uint64(1500), outCall.Amount)
	assert.Equal(t, nonce, outCall.TransferRef)

	// B's transfer_in arrives asynchronously after the proof returns.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(bLedger.snapshotInCalls()) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	in := bLedger.snapshotInCalls()
	require.Len(t, in, 1)
	assert.Equal(t, identity, in[0].IdentityID)
	assert.Equal(t, nonce, in[0].TransferRef)
	assert.Equal(t, uint64(1500), in[0].Amount)
}

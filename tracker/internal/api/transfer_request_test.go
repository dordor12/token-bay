package api_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	fed "github.com/token-bay/token-bay/shared/federation"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/api"
)

// stubFederation captures the inbound *tbproto.TransferRequest and lets a
// test choose what to return.
type stubFederation struct {
	got   *tbproto.TransferRequest
	resp  *tbproto.TransferProof
	err   error
	calls int
}

func (s *stubFederation) StartTransfer(_ context.Context, req *tbproto.TransferRequest) (*tbproto.TransferProof, error) {
	s.calls++
	if req != nil {
		s.got = proto.Clone(req).(*tbproto.TransferRequest)
	}
	return s.resp, s.err
}

// transferReqFixture returns a valid signed tbproto.TransferRequest plus
// the consumer keypair and the destination tracker key that the test
// router will use to derive its own MyTrackerID.
func transferReqFixture(t *testing.T) (*tbproto.TransferRequest, ed25519.PublicKey, ed25519.PublicKey) {
	t.Helper()
	conPub, conPriv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	dstPub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	var src [32]byte
	src[0] = 0x11
	var identity [32]byte
	identity[0] = 0x33
	var nonce [32]byte
	nonce[0] = 0x44
	dstID := sha256.Sum256(dstPub)

	fedReq := &fed.TransferProofRequest{
		SourceTrackerId: src[:],
		DestTrackerId:   dstID[:],
		IdentityId:      identity[:],
		Amount:          250,
		Nonce:           nonce[:],
		ConsumerPub:     conPub,
		Timestamp:       1714000000,
	}
	sig, err := fed.SignTransferProofRequest(conPriv, fedReq)
	require.NoError(t, err)

	return &tbproto.TransferRequest{
		IdentityId:      identity[:],
		Amount:          250,
		DestRegion:      "us-east-1",
		Nonce:           nonce[:],
		SourceTrackerId: src[:],
		ConsumerSig:     sig,
		ConsumerPub:     conPub,
		Timestamp:       1714000000,
	}, conPub, dstPub
}

func TestTransferRequest_NilFederation_NotImplemented(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_TRANSFER_REQUEST,
	})
	if resp.Error == nil || resp.Error.Code != "NOT_IMPLEMENTED" {
		t.Fatalf("got %+v", resp.Error)
	}
}

func TestTransferRequest_HappyPath_ReturnsProof(t *testing.T) {
	req, _, dstPub := transferReqFixture(t)
	stub := &stubFederation{
		resp: &tbproto.TransferProof{
			SourceChainTipHash: make([]byte, 32),
			SourceSeq:          7,
			TrackerSig:         make([]byte, 64),
		},
	}
	r, _ := api.NewRouter(api.Deps{
		Federation: stub,
		TrackerPub: dstPub,
	})
	payload, err := proto.Marshal(req)
	require.NoError(t, err)

	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_TRANSFER_REQUEST,
		Payload: payload,
	})
	require.Nil(t, resp.Error, "unexpected error: %+v", resp.Error)
	assert.Equal(t, tbproto.RpcStatus_RPC_STATUS_OK, resp.Status)
	assert.Equal(t, 1, stub.calls)

	var proof tbproto.TransferProof
	require.NoError(t, proto.Unmarshal(resp.Payload, &proof))
	assert.Equal(t, uint64(7), proof.SourceSeq)
}

func TestTransferRequest_TamperedConsumerSig_InvalidStatus(t *testing.T) {
	req, _, dstPub := transferReqFixture(t)
	// Flip a bit in the sig so verification fails.
	req.ConsumerSig[0] ^= 0xFF
	stub := &stubFederation{
		resp: &tbproto.TransferProof{
			SourceChainTipHash: make([]byte, 32),
			SourceSeq:          1,
			TrackerSig:         make([]byte, 64),
		},
	}
	r, _ := api.NewRouter(api.Deps{
		Federation: stub,
		TrackerPub: dstPub,
	})
	payload, err := proto.Marshal(req)
	require.NoError(t, err)
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_TRANSFER_REQUEST,
		Payload: payload,
	})
	require.NotNil(t, resp.Error)
	assert.Equal(t, tbproto.RpcStatus_RPC_STATUS_UNAUTHENTICATED, resp.Status)
	assert.Equal(t, 0, stub.calls, "federation must not be called on bad sig")
}

func TestTransferRequest_TamperedBody_InvalidStatus(t *testing.T) {
	req, _, dstPub := transferReqFixture(t)
	// Flip a bit in the body; sig will no longer match.
	req.Amount = req.Amount + 1
	stub := &stubFederation{}
	r, _ := api.NewRouter(api.Deps{Federation: stub, TrackerPub: dstPub})
	payload, err := proto.Marshal(req)
	require.NoError(t, err)
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_TRANSFER_REQUEST,
		Payload: payload,
	})
	require.NotNil(t, resp.Error)
	assert.Equal(t, tbproto.RpcStatus_RPC_STATUS_UNAUTHENTICATED, resp.Status)
	assert.Equal(t, 0, stub.calls)
}

func TestTransferRequest_ShortFields_Invalid(t *testing.T) {
	req, _, dstPub := transferReqFixture(t)
	req.IdentityId = req.IdentityId[:4] // wrong length

	r, _ := api.NewRouter(api.Deps{Federation: &stubFederation{}, TrackerPub: dstPub})
	payload, err := proto.Marshal(req)
	require.NoError(t, err)
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_TRANSFER_REQUEST,
		Payload: payload,
	})
	require.NotNil(t, resp.Error)
	assert.Equal(t, tbproto.RpcStatus_RPC_STATUS_INVALID, resp.Status)
}

func TestTransferRequest_BadPayload_Invalid(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{Federation: &stubFederation{}, TrackerPub: ed25519.PublicKey(make([]byte, 32))})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_TRANSFER_REQUEST,
		Payload: []byte{0xFF, 0xFF, 0xFF},
	})
	require.NotNil(t, resp.Error)
	assert.Equal(t, tbproto.RpcStatus_RPC_STATUS_INVALID, resp.Status)
}

func TestTransferRequest_FederationError_Propagates(t *testing.T) {
	req, _, dstPub := transferReqFixture(t)
	stub := &stubFederation{
		err: errors.New("federation: peer not connected"),
	}
	r, _ := api.NewRouter(api.Deps{
		Federation: stub,
		TrackerPub: dstPub,
		Debug:      true, // surface the wrapped error message
	})
	payload, err := proto.Marshal(req)
	require.NoError(t, err)
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_TRANSFER_REQUEST,
		Payload: payload,
	})
	require.NotNil(t, resp.Error)
	assert.Equal(t, tbproto.RpcStatus_RPC_STATUS_INTERNAL, resp.Status)
	assert.Contains(t, resp.Error.Message, "peer not connected")
	assert.Equal(t, 1, stub.calls)
}

package wire

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

func TestMarshalRequestRejectsHeartbeatMethod(t *testing.T) {
	_, err := MarshalRequest(tbproto.RpcMethod_RPC_METHOD_UNSPECIFIED, nil)
	require.Error(t, err)
}

func TestMarshalRequestNilPayloadOK(t *testing.T) {
	req, err := MarshalRequest(tbproto.RpcMethod_RPC_METHOD_STUN_ALLOCATE, nil)
	require.NoError(t, err)
	assert.Equal(t, tbproto.RpcMethod_RPC_METHOD_STUN_ALLOCATE, req.Method)
	assert.Empty(t, req.Payload)
}

func TestMarshalRoundTrip(t *testing.T) {
	src := &tbproto.BalanceRequest{IdentityId: make([]byte, 32)}
	req, err := MarshalRequest(tbproto.RpcMethod_RPC_METHOD_BALANCE, src)
	require.NoError(t, err)
	out := &tbproto.BalanceRequest{}
	require.NoError(t, UnmarshalResponse(&tbproto.RpcResponse{Payload: req.Payload}, out))
	assert.Equal(t, src.IdentityId, out.IdentityId)
}

func TestUnmarshalResponseNilDstOK(t *testing.T) {
	require.NoError(t, UnmarshalResponse(&tbproto.RpcResponse{Payload: []byte("ignored")}, nil))
}

func TestUnmarshalResponseNilResp(t *testing.T) {
	require.Error(t, UnmarshalResponse(nil, &tbproto.BalanceRequest{}))
}

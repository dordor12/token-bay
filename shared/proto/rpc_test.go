package proto

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestRpcRequestRoundTrip(t *testing.T) {
	in := &RpcRequest{Method: RpcMethod_RPC_METHOD_BROKER_REQUEST, Payload: []byte{1, 2, 3}}
	b, err := proto.Marshal(in)
	require.NoError(t, err)
	out := &RpcRequest{}
	require.NoError(t, proto.Unmarshal(b, out))
	assert.Equal(t, in.Method, out.Method)
	assert.Equal(t, in.Payload, out.Payload)
}

func TestRpcResponseRoundTrip(t *testing.T) {
	in := &RpcResponse{
		Status:  RpcStatus_RPC_STATUS_NO_CAPACITY,
		Payload: nil,
		Error:   &RpcError{Code: "no_capacity", Message: "all seeders busy"},
	}
	b, err := proto.Marshal(in)
	require.NoError(t, err)
	out := &RpcResponse{}
	require.NoError(t, proto.Unmarshal(b, out))
	assert.Equal(t, in.Status, out.Status)
	assert.Equal(t, in.Error.Code, out.Error.Code)
	assert.Equal(t, in.Error.Message, out.Error.Message)
}

func TestPushMessagesRoundTrip(t *testing.T) {
	cases := []proto.Message{
		&HeartbeatPing{Seq: 1, T: 100},
		&HeartbeatPong{Seq: 1},
		&OfferPush{ConsumerId: make([]byte, 32), EnvelopeHash: make([]byte, 32), Model: "claude-sonnet-4-6"},
		&OfferDecision{Accept: true, EphemeralPubkey: make([]byte, 32)},
		&SettlementPush{PreimageHash: make([]byte, 32), PreimageBody: []byte("body")},
	}
	for _, m := range cases {
		b, err := proto.Marshal(m)
		require.NoError(t, err)
		assert.NotNil(t, b)
	}
}

func TestRpcRequestGoldenBytes(t *testing.T) {
	r := &RpcRequest{
		Method:  RpcMethod_RPC_METHOD_BROKER_REQUEST,
		Payload: []byte("fixture"),
	}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(r)
	require.NoError(t, err)
	expected := readGoldenHex(t, "testdata/rpc_request_broker.golden.hex")
	assert.Equal(t, expected, b)
}

func TestRpcResponseGoldenBytes(t *testing.T) {
	r := &RpcResponse{
		Status:  RpcStatus_RPC_STATUS_OK,
		Payload: []byte("ok-payload"),
	}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(r)
	require.NoError(t, err)
	expected := readGoldenHex(t, "testdata/rpc_response_broker_ok.golden.hex")
	assert.Equal(t, expected, b)
}

func readGoldenHex(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	s := strings.TrimSpace(string(raw))
	out, err := hex.DecodeString(s)
	require.NoError(t, err)
	return out
}

package trackerclient

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

func TestStatusToErrOK(t *testing.T) {
	assert.NoError(t, statusToErr(tbproto.RpcStatus_RPC_STATUS_OK, nil))
}

func TestStatusToErrSentinels(t *testing.T) {
	cases := []struct {
		status   tbproto.RpcStatus
		sentinel error
	}{
		{tbproto.RpcStatus_RPC_STATUS_NO_CAPACITY, ErrNoCapacity},
		{tbproto.RpcStatus_RPC_STATUS_FROZEN, ErrFrozen},
		{tbproto.RpcStatus_RPC_STATUS_UNAUTHENTICATED, ErrUnauthenticated},
		{tbproto.RpcStatus_RPC_STATUS_INVALID, ErrInvalidResponse},
	}
	for _, c := range cases {
		err := statusToErr(c.status, &tbproto.RpcError{Code: "x-code", Message: "y-msg"})
		assert.True(t, errors.Is(err, c.sentinel), "want errors.Is %v", c.sentinel)

		var rpcErr *RpcError
		assert.True(t, errors.As(err, &rpcErr), "want errors.As to recover *RpcError for %v", c.status)
		assert.Equal(t, "x-code", rpcErr.Code)
		assert.Equal(t, "y-msg", rpcErr.Message)

		assert.Contains(t, err.Error(), "x-code")
		assert.Contains(t, err.Error(), "y-msg")
	}
}

func TestStatusToErrUnmappedFallsThroughToRpcError(t *testing.T) {
	cases := []tbproto.RpcStatus{
		tbproto.RpcStatus_RPC_STATUS_INTERNAL,
		tbproto.RpcStatus_RPC_STATUS_NOT_FOUND,
		tbproto.RpcStatus_RPC_STATUS_UNSPECIFIED,
	}
	for _, st := range cases {
		err := statusToErr(st, &tbproto.RpcError{Code: "boom", Message: "details"})
		var rpcErr *RpcError
		assert.True(t, errors.As(err, &rpcErr), "status=%v", st)
		assert.Equal(t, "boom", rpcErr.Code)
	}
}

func TestRpcErrorNilSafe(t *testing.T) {
	var e *RpcError
	assert.Equal(t, "<nil RpcError>", e.Error())
}

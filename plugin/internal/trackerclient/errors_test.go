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
		err := statusToErr(c.status, &tbproto.RpcError{Code: "x", Message: "y"})
		assert.True(t, errors.Is(err, c.sentinel), "want errors.Is %v", c.sentinel)
	}
}

func TestStatusToErrInternalKeepsRpcError(t *testing.T) {
	err := statusToErr(tbproto.RpcStatus_RPC_STATUS_INTERNAL, &tbproto.RpcError{Code: "boom", Message: "details"})
	var rpcErr *RpcError
	assert.True(t, errors.As(err, &rpcErr))
	assert.Equal(t, "boom", rpcErr.Code)
}

func TestRpcErrorNilSafe(t *testing.T) {
	var e *RpcError
	assert.Equal(t, "<nil RpcError>", e.Error())
}

package trackerclient

import (
	"errors"
	"fmt"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

var (
	ErrNotStarted       = errors.New("trackerclient: Start not called")
	ErrAlreadyStarted   = errors.New("trackerclient: already started")
	ErrClosed           = errors.New("trackerclient: client closed")
	ErrConnectionLost   = errors.New("trackerclient: connection lost mid-RPC")
	ErrIdentityMismatch = errors.New("trackerclient: tracker identity does not match pin")
	ErrInvalidEndpoint  = errors.New("trackerclient: invalid endpoint")
	ErrNoCapacity       = errors.New("trackerclient: tracker reports no capacity")
	ErrFrozen           = errors.New("trackerclient: identity frozen by reputation")
	ErrUnauthenticated  = errors.New("trackerclient: tracker rejected identity")
	ErrFrameTooLarge    = errors.New("trackerclient: framed message exceeds MaxFrameSize")
	ErrInvalidResponse  = errors.New("trackerclient: malformed RpcResponse")
	ErrAlpnMismatch     = errors.New("trackerclient: ALPN negotiation failed")
	ErrNoHandler        = errors.New("trackerclient: server pushed a stream but no handler is registered")
	ErrConfigInvalid    = errors.New("trackerclient: config invalid")
)

// RpcError wraps a tracker-side structured error.
//
//nolint:revive // TODO: rename to RPCError in a follow-up sweep that also updates the plan
type RpcError struct {
	Code    string
	Message string
}

func (e *RpcError) Error() string {
	if e == nil {
		return "<nil RpcError>"
	}
	return fmt.Sprintf("trackerclient: tracker error %q: %s", e.Code, e.Message)
}

// statusToErr maps an RpcStatus + RpcError to a typed Go error.
// Callers expect errors.Is(err, ErrNoCapacity) etc. to succeed.
func statusToErr(status tbproto.RpcStatus, e *tbproto.RpcError) error {
	if status == tbproto.RpcStatus_RPC_STATUS_OK {
		return nil
	}
	rpcErr := &RpcError{}
	if e != nil {
		rpcErr.Code = e.Code
		rpcErr.Message = e.Message
	}
	var sentinel error
	switch status {
	case tbproto.RpcStatus_RPC_STATUS_NO_CAPACITY:
		sentinel = ErrNoCapacity
	case tbproto.RpcStatus_RPC_STATUS_FROZEN:
		sentinel = ErrFrozen
	case tbproto.RpcStatus_RPC_STATUS_UNAUTHENTICATED:
		sentinel = ErrUnauthenticated
	case tbproto.RpcStatus_RPC_STATUS_INVALID:
		sentinel = ErrInvalidResponse
	default:
		// INTERNAL, NOT_FOUND, UNSPECIFIED — surface via RpcError only.
		// (UNSPECIFIED can arise from a buggy or older tracker emitting the
		// zero-value enum; we don't elevate it to a sentinel because it
		// signals a wire-protocol violation rather than a typed condition.)
		return rpcErr
	}
	return fmt.Errorf("%w: %w", sentinel, rpcErr)
}

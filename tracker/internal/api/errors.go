package api

import (
	"errors"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// apiErr is the concrete typed error returned by the Err* helpers.
// Dispatch uses errors.As to recover the declared status / code.
type apiErr struct {
	status tbproto.RpcStatus
	code   string
	msg    string
}

func (e *apiErr) Error() string { return e.code + ": " + e.msg }

// ErrInvalid signals a malformed request: bad payload, wrong field
// length, validation failure. Maps to RPC_STATUS_INVALID / "INVALID".
func ErrInvalid(msg string) error {
	return &apiErr{status: tbproto.RpcStatus_RPC_STATUS_INVALID, code: "INVALID", msg: msg}
}

// ErrNoCapacity signals the operation would exceed a quota or budget.
// Maps to RPC_STATUS_NO_CAPACITY / "NO_CAPACITY".
func ErrNoCapacity(msg string) error {
	return &apiErr{status: tbproto.RpcStatus_RPC_STATUS_NO_CAPACITY, code: "NO_CAPACITY", msg: msg}
}

// ErrFrozen signals the identity is in FROZEN state.
// Maps to RPC_STATUS_FROZEN / "FROZEN".
func ErrFrozen(msg string) error {
	return &apiErr{status: tbproto.RpcStatus_RPC_STATUS_FROZEN, code: "FROZEN", msg: msg}
}

// ErrNotFound signals a referenced entity does not exist.
// Maps to RPC_STATUS_NOT_FOUND / "NOT_FOUND".
func ErrNotFound(msg string) error {
	return &apiErr{status: tbproto.RpcStatus_RPC_STATUS_NOT_FOUND, code: "NOT_FOUND", msg: msg}
}

// ErrUnauthenticated signals the caller's identity could not be
// authenticated for this operation.
// Maps to RPC_STATUS_UNAUTHENTICATED / "UNAUTHENTICATED".
func ErrUnauthenticated(msg string) error {
	return &apiErr{status: tbproto.RpcStatus_RPC_STATUS_UNAUTHENTICATED, code: "UNAUTHENTICATED", msg: msg}
}

// ErrNotImplemented signals the handler is a Scope-2 stub awaiting its
// subsystem. Maps to RPC_STATUS_INTERNAL / "NOT_IMPLEMENTED".
func ErrNotImplemented(rpc string) error {
	return &apiErr{
		status: tbproto.RpcStatus_RPC_STATUS_INTERNAL,
		code:   "NOT_IMPLEMENTED",
		msg:    rpc + " handler awaiting subsystem",
	}
}

// OkResponse builds an RPC_STATUS_OK response with payload (may be nil).
func OkResponse(payload []byte) *tbproto.RpcResponse {
	return &tbproto.RpcResponse{
		Status:  tbproto.RpcStatus_RPC_STATUS_OK,
		Payload: payload,
	}
}

// ErrToResponse converts any error to an RpcResponse. apiErr maps to its
// declared status/code; other errors map to INTERNAL/INTERNAL with the
// message redacted to "internal error" unless debug is true.
func ErrToResponse(err error, debug bool) *tbproto.RpcResponse {
	if err == nil {
		return OkResponse(nil)
	}
	var ae *apiErr
	if errors.As(err, &ae) {
		return &tbproto.RpcResponse{
			Status: ae.status,
			Error:  &tbproto.RpcError{Code: ae.code, Message: ae.msg},
		}
	}
	msg := "internal error"
	if debug {
		msg = err.Error()
	}
	return &tbproto.RpcResponse{
		Status: tbproto.RpcStatus_RPC_STATUS_INTERNAL,
		Error:  &tbproto.RpcError{Code: "INTERNAL", Message: msg},
	}
}

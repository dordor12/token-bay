package api_test

import (
	"errors"
	"testing"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/api"
)

func TestErrInvalid_StatusMapping(t *testing.T) {
	resp := api.ErrToResponse(api.ErrInvalid("bad payload"), false)
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INVALID {
		t.Fatalf("status = %v, want INVALID", resp.Status)
	}
	if resp.Error == nil || resp.Error.Code != "INVALID" {
		t.Fatalf("error = %+v, want code=INVALID", resp.Error)
	}
}

func TestErrNoCapacity_StatusMapping(t *testing.T) {
	resp := api.ErrToResponse(api.ErrNoCapacity("full"), false)
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_NO_CAPACITY ||
		resp.Error.Code != "NO_CAPACITY" {
		t.Fatalf("got %+v / %+v", resp.Status, resp.Error)
	}
}

func TestErrFrozen_StatusMapping(t *testing.T) {
	resp := api.ErrToResponse(api.ErrFrozen("identity frozen"), false)
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_FROZEN ||
		resp.Error.Code != "FROZEN" {
		t.Fatalf("got %+v / %+v", resp.Status, resp.Error)
	}
}

func TestErrNotFound_StatusMapping(t *testing.T) {
	resp := api.ErrToResponse(api.ErrNotFound("identity"), false)
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_NOT_FOUND ||
		resp.Error.Code != "NOT_FOUND" {
		t.Fatalf("got %+v / %+v", resp.Status, resp.Error)
	}
}

func TestErrUnauthenticated_StatusMapping(t *testing.T) {
	resp := api.ErrToResponse(api.ErrUnauthenticated("bad sig"), false)
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_UNAUTHENTICATED ||
		resp.Error.Code != "UNAUTHENTICATED" {
		t.Fatalf("got %+v / %+v", resp.Status, resp.Error)
	}
}

func TestErrNotImplemented_StatusMapping(t *testing.T) {
	resp := api.ErrToResponse(api.ErrNotImplemented("broker_request"), false)
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INTERNAL ||
		resp.Error.Code != "NOT_IMPLEMENTED" {
		t.Fatalf("got %+v / %+v", resp.Status, resp.Error)
	}
}

func TestUnknownError_StatusMapping_RedactsAtNonDebug(t *testing.T) {
	resp := api.ErrToResponse(errors.New("inner secret"), false)
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INTERNAL {
		t.Fatalf("status = %v", resp.Status)
	}
	if resp.Error.Code != "INTERNAL" {
		t.Fatalf("code = %q", resp.Error.Code)
	}
	if resp.Error.Message == "inner secret" {
		t.Fatal("non-debug must redact internal error message")
	}
}

func TestUnknownError_StatusMapping_RevealsAtDebug(t *testing.T) {
	resp := api.ErrToResponse(errors.New("inner secret"), true)
	if resp.Error.Message != "inner secret" {
		t.Fatalf("debug message = %q, want inner secret", resp.Error.Message)
	}
}

func TestOkResponse_NoError(t *testing.T) {
	resp := api.OkResponse([]byte("payload"))
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status = %v", resp.Status)
	}
	if resp.Error != nil {
		t.Fatalf("error must be nil on OK; got %+v", resp.Error)
	}
	if string(resp.Payload) != "payload" {
		t.Fatalf("payload = %q", resp.Payload)
	}
}

func TestErrToResponse_NilErr_OK(t *testing.T) {
	resp := api.ErrToResponse(nil, false)
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status = %v", resp.Status)
	}
}

package api_test

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/api"
)

func newRC() *api.RequestCtx {
	return &api.RequestCtx{
		PeerID:     ids.IdentityID{1, 2, 3},
		RemoteAddr: netip.MustParseAddrPort("127.0.0.1:55001"),
		Now:        time.Unix(1700000000, 0),
		Logger:     zerolog.Nop(),
	}
}

func TestNewRouter_NilDeps_AllStubs(t *testing.T) {
	r, err := api.NewRouter(api.Deps{Logger: zerolog.Nop(), Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []tbproto.RpcMethod{
		tbproto.RpcMethod_RPC_METHOD_ENROLL,
		tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
		tbproto.RpcMethod_RPC_METHOD_BALANCE,
		tbproto.RpcMethod_RPC_METHOD_SETTLE,
		tbproto.RpcMethod_RPC_METHOD_USAGE_REPORT,
		tbproto.RpcMethod_RPC_METHOD_ADVERTISE,
		tbproto.RpcMethod_RPC_METHOD_TRANSFER_REQUEST,
		tbproto.RpcMethod_RPC_METHOD_STUN_ALLOCATE,
		tbproto.RpcMethod_RPC_METHOD_TURN_RELAY_OPEN,
	} {
		req := &tbproto.RpcRequest{Method: m}
		resp := r.Dispatch(context.Background(), newRC(), req)
		if resp.Status != tbproto.RpcStatus_RPC_STATUS_INTERNAL ||
			resp.Error == nil || resp.Error.Code != "NOT_IMPLEMENTED" {
			t.Errorf("method %v: got %+v / %+v, want INTERNAL/NOT_IMPLEMENTED",
				m, resp.Status, resp.Error)
		}
	}
}

func TestDispatch_UnknownMethod_Invalid(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{Logger: zerolog.Nop(), Now: time.Now})
	req := &tbproto.RpcRequest{Method: tbproto.RpcMethod(99)}
	resp := r.Dispatch(context.Background(), newRC(), req)
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INVALID ||
		resp.Error == nil || resp.Error.Code != "UNKNOWN_METHOD" {
		t.Fatalf("got %+v / %+v", resp.Status, resp.Error)
	}
}

func TestDispatch_MethodZero_Invalid(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{Logger: zerolog.Nop(), Now: time.Now})
	req := &tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_UNSPECIFIED}
	resp := r.Dispatch(context.Background(), newRC(), req)
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INVALID ||
		resp.Error == nil || resp.Error.Code != "METHOD_ZERO_RESERVED" {
		t.Fatalf("got %+v / %+v", resp.Status, resp.Error)
	}
}

func TestNewRouter_NowDefault(t *testing.T) {
	// nil Now should be replaced with time.Now; ensure no panic on Dispatch
	// (Dispatch reads rc.Now, not router.deps.Now, but the wiring is still
	// exercised here.)
	r, err := api.NewRouter(api.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_BALANCE,
	})
	if resp == nil {
		t.Fatal("nil response")
	}
}

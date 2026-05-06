package api_test

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/api"
	"github.com/token-bay/token-bay/tracker/internal/stunturn"
)

// fakeStunService satisfies api.StunTurnService — the union of
// stunReflector and turnAllocator.
type fakeStunService struct {
	saw netip.AddrPort
}

func (f *fakeStunService) ReflectAddr(remote netip.AddrPort) netip.AddrPort {
	f.saw = remote
	return remote
}

// Allocate satisfies turnAllocator. Returns an empty session — used by
// turn_relay_open tests via fakeTurnService.
func (f *fakeStunService) Allocate(_, _ ids.IdentityID, _ [16]byte, _ time.Time) (stunturn.Session, error) {
	return stunturn.Session{}, nil
}

func TestStunAllocate_OK_ReturnsRemoteAddr(t *testing.T) {
	fake := &fakeStunService{}
	r, _ := api.NewRouter(api.Deps{StunTurn: fake})
	rc := newRC() // RemoteAddr 127.0.0.1:55001

	body, _ := proto.Marshal(&tbproto.StunAllocateRequest{})
	resp := r.Dispatch(context.Background(), rc, &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_STUN_ALLOCATE, Payload: body,
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status = %v: %+v", resp.Status, resp.Error)
	}
	var out tbproto.StunAllocateResponse
	if err := proto.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.ExternalAddr != "127.0.0.1:55001" {
		t.Fatalf("external_addr = %q", out.ExternalAddr)
	}
	if fake.saw != rc.RemoteAddr {
		t.Fatalf("ReflectAddr saw %v, want %v", fake.saw, rc.RemoteAddr)
	}
}

func TestStunAllocate_GarbagePayload_Invalid(t *testing.T) {
	fake := &fakeStunService{}
	r, _ := api.NewRouter(api.Deps{StunTurn: fake})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_STUN_ALLOCATE,
		Payload: []byte{0xff, 0xff, 0xff},
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INVALID {
		t.Fatalf("status = %v", resp.Status)
	}
}

func TestStunAllocate_EmptyPayload_OK(t *testing.T) {
	fake := &fakeStunService{}
	r, _ := api.NewRouter(api.Deps{StunTurn: fake})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_STUN_ALLOCATE,
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status = %v: %+v", resp.Status, resp.Error)
	}
}

func TestStunAllocate_NilStunTurn_NotImplemented(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{}) // StunTurn nil
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_STUN_ALLOCATE,
	})
	if resp.Error == nil || resp.Error.Code != "NOT_IMPLEMENTED" {
		t.Fatalf("got %+v", resp.Error)
	}
}

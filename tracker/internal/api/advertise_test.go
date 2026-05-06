package api_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/api"
	"github.com/token-bay/token-bay/tracker/internal/registry"
)

type fakeAdvertiseRegistry struct {
	gotID    ids.IdentityID
	gotCaps  registry.Capabilities
	gotAvail bool
	gotHR    float64
	retErr   error
}

func (f *fakeAdvertiseRegistry) Advertise(id ids.IdentityID, c registry.Capabilities, a bool, h float64) error {
	f.gotID, f.gotCaps, f.gotAvail, f.gotHR = id, c, a, h
	return f.retErr
}

func TestAdvertise_OK_PassesArgsThrough(t *testing.T) {
	fake := &fakeAdvertiseRegistry{}
	r, _ := api.NewRouter(api.Deps{Registry: fake})

	body, _ := proto.Marshal(&tbproto.Advertisement{
		Models:     []string{"claude-opus-4-7"},
		MaxContext: 200_000,
		Available:  true,
		Headroom:   0.42,
		Tiers:      0x1, // standard
	})
	rc := newRC()
	resp := r.Dispatch(context.Background(), rc, &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_ADVERTISE, Payload: body,
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status = %v: %+v", resp.Status, resp.Error)
	}
	if fake.gotID != rc.PeerID {
		t.Errorf("forwarded id = %x, want %x", fake.gotID, rc.PeerID)
	}
	if !fake.gotAvail || fake.gotHR < 0.41 || fake.gotHR > 0.43 {
		t.Errorf("forwarded avail/hr: %v/%f", fake.gotAvail, fake.gotHR)
	}
	if len(fake.gotCaps.Tiers) != 1 ||
		fake.gotCaps.Tiers[0] != tbproto.PrivacyTier_PRIVACY_TIER_STANDARD {
		t.Errorf("tiers = %+v", fake.gotCaps.Tiers)
	}
}

func TestAdvertise_TEEBitmask(t *testing.T) {
	fake := &fakeAdvertiseRegistry{}
	r, _ := api.NewRouter(api.Deps{Registry: fake})
	body, _ := proto.Marshal(&tbproto.Advertisement{
		Models:    []string{"x"},
		Available: true,
		Tiers:     0x3, // standard | tee
	})
	r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_ADVERTISE, Payload: body,
	})
	if len(fake.gotCaps.Tiers) != 2 {
		t.Fatalf("tiers = %+v", fake.gotCaps.Tiers)
	}
}

func TestAdvertise_RegistryError_Internal(t *testing.T) {
	fake := &fakeAdvertiseRegistry{retErr: errors.New("registry full")}
	r, _ := api.NewRouter(api.Deps{Registry: fake})
	body, _ := proto.Marshal(&tbproto.Advertisement{Models: []string{"x"}})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_ADVERTISE, Payload: body,
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INTERNAL {
		t.Fatalf("status = %v", resp.Status)
	}
}

func TestAdvertise_GarbagePayload_Invalid(t *testing.T) {
	fake := &fakeAdvertiseRegistry{}
	r, _ := api.NewRouter(api.Deps{Registry: fake})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_ADVERTISE, Payload: []byte{0xff, 0xff},
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INVALID {
		t.Fatalf("status = %v", resp.Status)
	}
}

func TestAdvertise_NilRegistry_NotImplemented(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{}) // Registry nil
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_ADVERTISE,
	})
	if resp.Error == nil || resp.Error.Code != "NOT_IMPLEMENTED" {
		t.Fatalf("got %+v", resp.Error)
	}
}

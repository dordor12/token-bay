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

// fakeTurnService satisfies api.StunTurnService and lets the test inject
// the Allocate return value.
type fakeTurnService struct {
	sess   stunturn.Session
	retErr error
}

func (f *fakeTurnService) ReflectAddr(addr netip.AddrPort) netip.AddrPort { return addr }

func (f *fakeTurnService) Allocate(_, _ ids.IdentityID, _ [16]byte, _ time.Time) (stunturn.Session, error) {
	if f.retErr != nil {
		return stunturn.Session{}, f.retErr
	}
	return f.sess, nil
}

func TestTurnRelayOpen_OK(t *testing.T) {
	tok := stunturn.Token{0xab, 0xcd, 0xef}
	fake := &fakeTurnService{sess: stunturn.Session{Token: tok, SessionID: 1}}
	r, _ := api.NewRouter(api.Deps{StunTurn: fake})

	body, _ := proto.Marshal(&tbproto.TurnRelayOpenRequest{
		SessionId: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
	})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_TURN_RELAY_OPEN, Payload: body,
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status = %v: %+v", resp.Status, resp.Error)
	}
	var out tbproto.TurnRelayOpenResponse
	if err := proto.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.RelayEndpoint == "" || len(out.Token) == 0 {
		t.Fatalf("response = %+v", &out)
	}
}

func TestTurnRelayOpen_AllocatorThrottled_NoCapacity(t *testing.T) {
	fake := &fakeTurnService{retErr: stunturn.ErrThrottled}
	r, _ := api.NewRouter(api.Deps{StunTurn: fake})
	body, _ := proto.Marshal(&tbproto.TurnRelayOpenRequest{
		SessionId: make([]byte, 16),
	})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_TURN_RELAY_OPEN, Payload: body,
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_NO_CAPACITY {
		t.Fatalf("status = %v", resp.Status)
	}
}

func TestTurnRelayOpen_AllocatorDuplicate_Invalid(t *testing.T) {
	fake := &fakeTurnService{retErr: stunturn.ErrDuplicateRequest}
	r, _ := api.NewRouter(api.Deps{StunTurn: fake})
	body, _ := proto.Marshal(&tbproto.TurnRelayOpenRequest{
		SessionId: make([]byte, 16),
	})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_TURN_RELAY_OPEN, Payload: body,
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INVALID {
		t.Fatalf("status = %v", resp.Status)
	}
}

func TestTurnRelayOpen_BadSessionIDLength_Invalid(t *testing.T) {
	fake := &fakeTurnService{}
	r, _ := api.NewRouter(api.Deps{StunTurn: fake})
	body, _ := proto.Marshal(&tbproto.TurnRelayOpenRequest{SessionId: []byte{0x01}})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_TURN_RELAY_OPEN, Payload: body,
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INVALID {
		t.Fatalf("status = %v", resp.Status)
	}
}

func TestTurnRelayOpen_NilStunTurn_NotImplemented(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{})
	body, _ := proto.Marshal(&tbproto.TurnRelayOpenRequest{
		SessionId: make([]byte, 16),
	})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_TURN_RELAY_OPEN, Payload: body,
	})
	if resp.Error == nil || resp.Error.Code != "NOT_IMPLEMENTED" {
		t.Fatalf("got %+v", resp.Error)
	}
}

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
	"github.com/token-bay/token-bay/tracker/internal/broker"
	"github.com/token-bay/token-bay/tracker/internal/stunturn"
)

// fakeTurnService satisfies api.StunTurnService and lets the test inject
// the Allocate return value plus capture the (consumer, seeder) the handler
// passes through.
type fakeTurnService struct {
	sess         stunturn.Session
	retErr       error
	gotConsumer  ids.IdentityID
	gotSeeder    ids.IdentityID
	gotRequestID [16]byte
}

func (f *fakeTurnService) ReflectAddr(addr netip.AddrPort) netip.AddrPort { return addr }

func (f *fakeTurnService) Allocate(consumer, seeder ids.IdentityID, requestID [16]byte, _ time.Time) (stunturn.Session, error) {
	f.gotConsumer = consumer
	f.gotSeeder = seeder
	f.gotRequestID = requestID
	if f.retErr != nil {
		return stunturn.Session{}, f.retErr
	}
	return f.sess, nil
}

// fakeBrokerLookup satisfies api.BrokerService: only LookupAssignment is
// exercised here — Submit / RegisterQueued are unreachable on the
// turn_relay_open path.
type fakeBrokerLookup struct {
	consumer ids.IdentityID
	seeder   ids.IdentityID
	ok       bool
}

func (f *fakeBrokerLookup) Submit(context.Context, *tbproto.EnvelopeSigned) (*broker.Result, error) {
	return nil, nil
}

func (f *fakeBrokerLookup) RegisterQueued(*tbproto.EnvelopeSigned, [16]byte, func(*broker.Result)) {
}

func (f *fakeBrokerLookup) CancelQueued([16]byte) {}

func (f *fakeBrokerLookup) LookupAssignment([16]byte) (ids.IdentityID, ids.IdentityID, bool) {
	return f.consumer, f.seeder, f.ok
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

func TestTurnRelayOpen_BrokerLookup_PassesAssignedSeeder(t *testing.T) {
	tok := stunturn.Token{0xab}
	fake := &fakeTurnService{sess: stunturn.Session{Token: tok}}
	// rc.PeerID is {1,2,3}; consumer matches caller, seeder differs.
	consumer := ids.IdentityID{1, 2, 3}
	seeder := ids.IdentityID{9, 9, 9}
	bro := &fakeBrokerLookup{consumer: consumer, seeder: seeder, ok: true}
	r, _ := api.NewRouter(api.Deps{StunTurn: fake, Broker: bro, RelayPublicAddr: "relay.example:3479"})

	rid := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	body, _ := proto.Marshal(&tbproto.TurnRelayOpenRequest{SessionId: rid})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_TURN_RELAY_OPEN, Payload: body,
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status = %v: %+v", resp.Status, resp.Error)
	}
	if fake.gotConsumer != consumer || fake.gotSeeder != seeder {
		t.Fatalf("Allocate got (consumer=%x, seeder=%x); want (%x, %x)",
			fake.gotConsumer, fake.gotSeeder, consumer, seeder)
	}
	var out tbproto.TurnRelayOpenResponse
	_ = proto.Unmarshal(resp.Payload, &out)
	if out.RelayEndpoint != "relay.example:3479" {
		t.Fatalf("relay_endpoint = %q, want injected addr", out.RelayEndpoint)
	}
}

func TestTurnRelayOpen_BrokerLookup_UnknownReservation_Invalid(t *testing.T) {
	fake := &fakeTurnService{}
	bro := &fakeBrokerLookup{ok: false}
	r, _ := api.NewRouter(api.Deps{StunTurn: fake, Broker: bro})

	body, _ := proto.Marshal(&tbproto.TurnRelayOpenRequest{SessionId: make([]byte, 16)})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_TURN_RELAY_OPEN, Payload: body,
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INVALID {
		t.Fatalf("status = %v: %+v", resp.Status, resp.Error)
	}
}

func TestTurnRelayOpen_BrokerLookup_CallerNotParty_Unauthenticated(t *testing.T) {
	fake := &fakeTurnService{sess: stunturn.Session{Token: stunturn.Token{0x01}}}
	// rc.PeerID is {1,2,3}; broker says request belongs to other peers.
	bro := &fakeBrokerLookup{
		consumer: ids.IdentityID{0xAA},
		seeder:   ids.IdentityID{0xBB},
		ok:       true,
	}
	r, _ := api.NewRouter(api.Deps{StunTurn: fake, Broker: bro})

	body, _ := proto.Marshal(&tbproto.TurnRelayOpenRequest{SessionId: make([]byte, 16)})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_TURN_RELAY_OPEN, Payload: body,
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_UNAUTHENTICATED {
		t.Fatalf("status = %v: %+v", resp.Status, resp.Error)
	}
}

func TestTurnRelayOpen_NoBroker_SelfLoopFallback(t *testing.T) {
	fake := &fakeTurnService{sess: stunturn.Session{Token: stunturn.Token{0x42}}}
	r, _ := api.NewRouter(api.Deps{StunTurn: fake})

	body, _ := proto.Marshal(&tbproto.TurnRelayOpenRequest{SessionId: make([]byte, 16)})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_TURN_RELAY_OPEN, Payload: body,
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status = %v: %+v", resp.Status, resp.Error)
	}
	peer := ids.IdentityID{1, 2, 3}
	if fake.gotConsumer != peer || fake.gotSeeder != peer {
		t.Fatalf("scope-2 self-loop: got (consumer=%x, seeder=%x); want both = %x",
			fake.gotConsumer, fake.gotSeeder, peer)
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

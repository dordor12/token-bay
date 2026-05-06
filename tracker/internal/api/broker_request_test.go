package api_test

import (
	"context"
	"testing"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/api"
)

// fakeBrokerSubmitter satisfies api.BrokerService. The Scope-2 stub
// ignores it, but the type is used to verify install* preserves the
// stub even when Deps.Broker is wired.
type fakeBrokerSubmitter struct{}

func (fakeBrokerSubmitter) Submit(_ context.Context, _ *tbproto.EnvelopeSigned) (*tbproto.BrokerResponse, error) {
	return nil, nil
}

func TestBrokerRequest_NilBroker_NotImplemented(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
	})
	if resp.Error == nil || resp.Error.Code != "NOT_IMPLEMENTED" {
		t.Fatalf("got %+v", resp.Error)
	}
}

func TestBrokerRequest_WiredBroker_StillNotImplemented(t *testing.T) {
	// Scope-2 contract: the install closure short-circuits to the stub
	// even when Deps.Broker is non-nil. Regression test for the future
	// broker PR — when the install body changes to the real handler,
	// THIS test changes too.
	r, _ := api.NewRouter(api.Deps{Broker: fakeBrokerSubmitter{}})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
	})
	if resp.Error == nil || resp.Error.Code != "NOT_IMPLEMENTED" {
		t.Fatalf("got %+v", resp.Error)
	}
}

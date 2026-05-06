package api_test

import (
	"context"
	"testing"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/api"
)

// fakeFederation satisfies api.FederationService.
type fakeFederation struct{}

func (fakeFederation) StartTransfer(_ context.Context, _ *tbproto.TransferRequest) (*tbproto.TransferProof, error) {
	return nil, nil
}

func TestTransferRequest_NilFederation_NotImplemented(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_TRANSFER_REQUEST,
	})
	if resp.Error == nil || resp.Error.Code != "NOT_IMPLEMENTED" {
		t.Fatalf("got %+v", resp.Error)
	}
}

func TestTransferRequest_WiredFederation_StillNotImplemented(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{Federation: fakeFederation{}})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_TRANSFER_REQUEST,
	})
	if resp.Error == nil || resp.Error.Code != "NOT_IMPLEMENTED" {
		t.Fatalf("got %+v", resp.Error)
	}
}

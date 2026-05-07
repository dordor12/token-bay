package api_test

import (
	"bytes"
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/api"
	"github.com/token-bay/token-bay/tracker/internal/broker"
)

func validSettleRequestBytes(t *testing.T) []byte {
	t.Helper()
	b, err := proto.Marshal(&tbproto.SettleRequest{
		PreimageHash: bytes.Repeat([]byte{0xDE}, 32),
		ConsumerSig:  bytes.Repeat([]byte{0xAD}, 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSettle_NotImplemented(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_SETTLE,
	})
	if resp.Error == nil || resp.Error.Code != "NOT_IMPLEMENTED" {
		t.Fatalf("got %+v", resp.Error)
	}
}

func TestSettle_OK_ReturnsAck(t *testing.T) {
	svc := &fakeSettlementService{}
	r, _ := api.NewRouter(api.Deps{Settlement: svc})

	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_SETTLE,
		Payload: validSettleRequestBytes(t),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status=%v error=%+v", resp.Status, resp.Error)
	}
}

func TestSettle_UnknownPreimage_NotFound(t *testing.T) {
	svc := &fakeSettlementService{settleErr: broker.ErrUnknownPreimage}
	r, _ := api.NewRouter(api.Deps{Settlement: svc})

	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_SETTLE,
		Payload: validSettleRequestBytes(t),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_NOT_FOUND {
		t.Fatalf("status=%v error=%+v", resp.Status, resp.Error)
	}
}

func TestSettle_DuplicateSettle_Invalid(t *testing.T) {
	svc := &fakeSettlementService{settleErr: broker.ErrDuplicateSettle}
	r, _ := api.NewRouter(api.Deps{Settlement: svc})

	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_SETTLE,
		Payload: validSettleRequestBytes(t),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INVALID {
		t.Fatalf("status=%v error=%+v", resp.Status, resp.Error)
	}
	if resp.Error.Code != "INVALID" {
		t.Errorf("code=%q want INVALID", resp.Error.Code)
	}
}

func TestSettle_BadPreimageHash_Invalid(t *testing.T) {
	b, _ := proto.Marshal(&tbproto.SettleRequest{
		PreimageHash: bytes.Repeat([]byte{0x01}, 16), // wrong length
	})
	svc := &fakeSettlementService{}
	r, _ := api.NewRouter(api.Deps{Settlement: svc})

	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_SETTLE,
		Payload: b,
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INVALID {
		t.Fatalf("status=%v", resp.Status)
	}
}

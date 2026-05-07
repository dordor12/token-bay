package api_test

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/api"
	"github.com/token-bay/token-bay/tracker/internal/broker"
	"github.com/token-bay/token-bay/tracker/internal/session"
)

// fakeSettlementService satisfies api.SettlementService (usageReportHandler
// + settleHandler).
type fakeSettlementService struct {
	usageErr  error
	usageAck  *tbproto.UsageAck
	settleErr error
	settleAck *tbproto.SettleAck
}

func (f *fakeSettlementService) HandleUsageReport(_ context.Context, _ ids.IdentityID, _ *tbproto.UsageReport) (*tbproto.UsageAck, error) {
	if f.usageAck != nil {
		return f.usageAck, f.usageErr
	}
	return &tbproto.UsageAck{}, f.usageErr
}

func (f *fakeSettlementService) HandleSettle(_ context.Context, _ ids.IdentityID, _ *tbproto.SettleRequest) (*tbproto.SettleAck, error) {
	if f.settleAck != nil {
		return f.settleAck, f.settleErr
	}
	return &tbproto.SettleAck{}, f.settleErr
}

func validUsageReportBytes(t *testing.T) []byte {
	t.Helper()
	b, err := proto.Marshal(&tbproto.UsageReport{
		RequestId:    make([]byte, 16),
		Model:        "claude-3-haiku-20240307",
		InputTokens:  100,
		OutputTokens: 200,
		SeederSig:    make([]byte, 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUsageReport_NotImplemented(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_USAGE_REPORT,
	})
	if resp.Error == nil || resp.Error.Code != "NOT_IMPLEMENTED" {
		t.Fatalf("got %+v", resp.Error)
	}
}

func TestUsageReport_OK_ReturnsAck(t *testing.T) {
	svc := &fakeSettlementService{}
	r, _ := api.NewRouter(api.Deps{Settlement: svc})

	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_USAGE_REPORT,
		Payload: validUsageReportBytes(t),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status=%v error=%+v", resp.Status, resp.Error)
	}
}

func TestUsageReport_UnknownRequest_NotFound(t *testing.T) {
	svc := &fakeSettlementService{usageErr: session.ErrUnknownRequest}
	r, _ := api.NewRouter(api.Deps{Settlement: svc})

	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_USAGE_REPORT,
		Payload: validUsageReportBytes(t),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_NOT_FOUND {
		t.Fatalf("status=%v error=%+v", resp.Status, resp.Error)
	}
}

func TestUsageReport_SeederMismatch_Invalid(t *testing.T) {
	svc := &fakeSettlementService{usageErr: broker.ErrSeederMismatch}
	r, _ := api.NewRouter(api.Deps{Settlement: svc})

	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_USAGE_REPORT,
		Payload: validUsageReportBytes(t),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INVALID {
		t.Fatalf("status=%v error=%+v", resp.Status, resp.Error)
	}
	if resp.Error.Code != "INVALID" {
		t.Errorf("code=%q want INVALID", resp.Error.Code)
	}
}

func TestUsageReport_CostOverspend_Invalid(t *testing.T) {
	svc := &fakeSettlementService{usageErr: broker.ErrCostOverspend}
	r, _ := api.NewRouter(api.Deps{Settlement: svc})

	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_USAGE_REPORT,
		Payload: validUsageReportBytes(t),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INVALID {
		t.Fatalf("status=%v error=%+v", resp.Status, resp.Error)
	}
}

func TestUsageReport_MissingRequestID_Invalid(t *testing.T) {
	b, _ := proto.Marshal(&tbproto.UsageReport{
		Model: "claude-3-haiku-20240307",
		// RequestId is empty — fails the 16-byte check
	})
	svc := &fakeSettlementService{}
	r, _ := api.NewRouter(api.Deps{Settlement: svc})

	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_USAGE_REPORT,
		Payload: b,
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INVALID {
		t.Fatalf("status=%v", resp.Status)
	}
}

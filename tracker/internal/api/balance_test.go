package api_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/api"
)

type fakeBalanceLedger struct {
	gotID  []byte
	resp   *tbproto.SignedBalanceSnapshot
	retErr error
}

func (f *fakeBalanceLedger) SignedBalance(_ context.Context, id []byte) (*tbproto.SignedBalanceSnapshot, error) {
	f.gotID = append([]byte(nil), id...)
	return f.resp, f.retErr
}

// IssueStarterGrant — present so *fakeBalanceLedger satisfies
// LedgerService (balanceLedger ∪ enrollLedger). Not exercised here.
func (f *fakeBalanceLedger) IssueStarterGrant(_ context.Context, _ []byte, _ uint64) (*tbproto.Entry, error) {
	return nil, errors.New("not used in balance tests")
}

func balanceRequestBytes(t *testing.T, id []byte) []byte {
	t.Helper()
	b, err := proto.Marshal(&tbproto.BalanceRequest{IdentityId: id})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBalance_OK(t *testing.T) {
	id32 := bytes.Repeat([]byte{0xab}, 32)
	want := &tbproto.SignedBalanceSnapshot{
		Body: &tbproto.BalanceSnapshotBody{
			IdentityId:   id32,
			Credits:      42,
			IssuedAt:     1700000000,
			ExpiresAt:    1700000600,
			ChainTipHash: bytes.Repeat([]byte{0}, 32),
		},
		TrackerSig: bytes.Repeat([]byte{0xcc}, 64),
	}
	fake := &fakeBalanceLedger{resp: want}
	r, _ := api.NewRouter(api.Deps{Ledger: fake})

	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BALANCE,
		Payload: balanceRequestBytes(t, id32),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status = %v: %+v", resp.Status, resp.Error)
	}
	var got tbproto.SignedBalanceSnapshot
	if err := proto.Unmarshal(resp.Payload, &got); err != nil {
		t.Fatal(err)
	}
	if got.Body.Credits != 42 {
		t.Fatalf("credits = %d, want 42", got.Body.Credits)
	}
	if !bytes.Equal(fake.gotID, id32) {
		t.Fatalf("forwarded id = %x", fake.gotID)
	}
}

func TestBalance_LedgerError_Internal(t *testing.T) {
	fake := &fakeBalanceLedger{retErr: errors.New("ledger boom")}
	r, _ := api.NewRouter(api.Deps{Ledger: fake})

	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BALANCE,
		Payload: balanceRequestBytes(t, bytes.Repeat([]byte{0x01}, 32)),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INTERNAL ||
		resp.Error.Code != "INTERNAL" {
		t.Fatalf("got %+v / %+v", resp.Status, resp.Error)
	}
}

func TestBalance_BadIdentityLength_Invalid(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{Ledger: &fakeBalanceLedger{}})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BALANCE,
		Payload: balanceRequestBytes(t, []byte{0x01}),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INVALID {
		t.Fatalf("status = %v", resp.Status)
	}
}

func TestBalance_GarbagePayload_Invalid(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{Ledger: &fakeBalanceLedger{}})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BALANCE,
		Payload: []byte{0xff, 0xff},
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INVALID {
		t.Fatalf("status = %v", resp.Status)
	}
}

func TestBalance_NilLedger_NotImplemented(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{}) // Ledger nil
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BALANCE,
		Payload: balanceRequestBytes(t, bytes.Repeat([]byte{0x01}, 32)),
	})
	if resp.Error == nil || resp.Error.Code != "NOT_IMPLEMENTED" {
		t.Fatalf("got %+v", resp.Error)
	}
}

package api

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)

// balanceLedger is the slice of ledger this handler needs.
type balanceLedger interface {
	SignedBalance(ctx context.Context, identityID []byte) (*tbproto.SignedBalanceSnapshot, error)
}

// installBalance returns the live balance handler when Deps.Ledger
// satisfies balanceLedger; otherwise returns the Scope-2 stub.
func (r *Router) installBalance() handlerFunc {
	if r.deps.Ledger == nil {
		return notImpl("balance")
	}
	led, ok := r.deps.Ledger.(balanceLedger)
	if !ok {
		return notImpl("balance")
	}
	return func(ctx context.Context, _ *RequestCtx, payloadBytes []byte) (*tbproto.RpcResponse, error) {
		var req tbproto.BalanceRequest
		if err := proto.Unmarshal(payloadBytes, &req); err != nil {
			return nil, ErrInvalid(fmt.Sprintf("BalanceRequest: %v", err))
		}
		if len(req.IdentityId) != 32 {
			return nil, ErrInvalid(fmt.Sprintf("identity_id length %d, want 32", len(req.IdentityId)))
		}
		snap, err := led.SignedBalance(ctx, req.IdentityId)
		if err != nil {
			return nil, fmt.Errorf("balance: %w", err)
		}
		out, err := signing.DeterministicMarshal(snap)
		if err != nil {
			return nil, fmt.Errorf("balance: marshal snapshot: %w", err)
		}
		return OkResponse(out), nil
	}
}

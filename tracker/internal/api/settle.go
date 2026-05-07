package api

import (
	"context"

	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// settleHandler is the narrow interface the settle handler needs.
type settleHandler interface {
	HandleSettle(ctx context.Context, peerID ids.IdentityID, r *tbproto.SettleRequest) (*tbproto.SettleAck, error)
}

// installSettle wires the live settle handler when Deps.Settlement satisfies
// settleHandler; otherwise returns the ErrNotImplemented stub.
func (r *Router) installSettle() handlerFunc {
	if r.deps.Settlement == nil {
		return notImpl("settle")
	}
	h, ok := r.deps.Settlement.(settleHandler)
	if !ok {
		return notImpl("settle")
	}
	return func(ctx context.Context, rc *RequestCtx, payloadBytes []byte) (*tbproto.RpcResponse, error) {
		var req tbproto.SettleRequest
		if err := proto.Unmarshal(payloadBytes, &req); err != nil {
			return nil, ErrInvalid("SettleRequest: " + err.Error())
		}
		if len(req.PreimageHash) != 32 {
			return nil, ErrInvalid("preimage_hash length")
		}
		ack, err := h.HandleSettle(ctx, rc.PeerID, &req)
		if err != nil {
			return nil, mapSettlementError(err)
		}
		ackBytes, _ := proto.Marshal(ack)
		return OkResponse(ackBytes), nil
	}
}

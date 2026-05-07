package api

import (
	"context"
	"errors"

	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/broker"
	"github.com/token-bay/token-bay/tracker/internal/session"
)

// usageReportHandler is the narrow interface the usage_report handler needs.
type usageReportHandler interface {
	HandleUsageReport(ctx context.Context, peerID ids.IdentityID, r *tbproto.UsageReport) (*tbproto.UsageAck, error)
}

// installUsageReport wires the live usage_report handler when Deps.Settlement
// satisfies usageReportHandler; otherwise returns the ErrNotImplemented stub.
func (r *Router) installUsageReport() handlerFunc {
	if r.deps.Settlement == nil {
		return notImpl("usage_report")
	}
	h, ok := r.deps.Settlement.(usageReportHandler)
	if !ok {
		return notImpl("usage_report")
	}
	return func(ctx context.Context, rc *RequestCtx, payloadBytes []byte) (*tbproto.RpcResponse, error) {
		var report tbproto.UsageReport
		if err := proto.Unmarshal(payloadBytes, &report); err != nil {
			return nil, ErrInvalid("UsageReport: " + err.Error())
		}
		if len(report.RequestId) != 16 {
			return nil, ErrInvalid("UsageReport.request_id: want 16 bytes")
		}
		if report.Model == "" {
			return nil, ErrInvalid("UsageReport.model: empty")
		}
		ack, err := h.HandleUsageReport(ctx, rc.PeerID, &report)
		if err != nil {
			return nil, mapSettlementError(err)
		}
		ackBytes, _ := proto.Marshal(ack)
		return OkResponse(ackBytes), nil
	}
}

// mapSettlementError translates broker package sentinels to RPC status codes.
func mapSettlementError(err error) error {
	switch {
	case errors.Is(err, session.ErrUnknownRequest):
		return ErrNotFound("unknown request_id")
	case errors.Is(err, broker.ErrSeederMismatch):
		return ErrInvalid("SEEDER_MISMATCH")
	case errors.Is(err, broker.ErrModelMismatch):
		return ErrInvalid("MODEL_MISMATCH")
	case errors.Is(err, broker.ErrCostOverspend):
		return ErrInvalid("COST_OVERSPEND")
	case errors.Is(err, broker.ErrSeederSigInvalid):
		return ErrInvalid("SEEDER_SIG_INVALID")
	case errors.Is(err, session.ErrIllegalTransition):
		return ErrInvalid("INVALID_STATE")
	case errors.Is(err, broker.ErrUnknownPreimage):
		return ErrNotFound("unknown preimage_hash")
	case errors.Is(err, broker.ErrDuplicateSettle):
		return ErrInvalid("DUPLICATE_SETTLE")
	default:
		return err // generic INTERNAL
	}
}

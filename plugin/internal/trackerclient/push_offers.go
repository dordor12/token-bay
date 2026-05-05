package trackerclient

import (
	"context"
	"errors"
	"io"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/wire"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// handleOffer reads a single OfferPush from stream, dispatches to h, and
// writes the OfferDecision back. Server-pushed offer streams are
// short-lived: one push, one decision, close.
func handleOffer(ctx context.Context, stream transport.Stream, h OfferHandler, maxFrameSize int) {
	var push tbproto.OfferPush
	if err := wire.Read(stream, &push, maxFrameSize); err != nil {
		if !errors.Is(err, io.EOF) {
			_ = wire.Write(stream, &tbproto.OfferDecision{
				Accept: false, RejectReason: "trackerclient: invalid push shape",
			}, maxFrameSize)
		}
		return
	}
	if err := tbproto.ValidateOfferPush(&push); err != nil {
		_ = wire.Write(stream, &tbproto.OfferDecision{
			Accept: false, RejectReason: err.Error(),
		}, maxFrameSize)
		return
	}

	var consumerID ids.IdentityID
	copy(consumerID[:], push.ConsumerId)
	var envHash [32]byte
	copy(envHash[:], push.EnvelopeHash)
	decision, err := h.HandleOffer(ctx, &Offer{
		ConsumerID:      consumerID,
		EnvelopeHash:    envHash,
		Model:           push.Model,
		MaxInputTokens:  push.MaxInputTokens,
		MaxOutputTokens: push.MaxOutputTokens,
	})
	pb := &tbproto.OfferDecision{Accept: decision.Accept}
	if decision.Accept {
		pb.EphemeralPubkey = decision.EphemeralPubkey
	} else {
		pb.RejectReason = decision.RejectReason
		if err != nil {
			pb.RejectReason = err.Error()
		}
	}
	_ = wire.Write(stream, pb, maxFrameSize)
}

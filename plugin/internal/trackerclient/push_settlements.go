package trackerclient

import (
	"context"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/wire"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// Push-stream class tags: a single byte the server writes before the
// first proto frame, telling the acceptor which handler to dispatch to.
const (
	pushTagOffer        = 0x01
	pushTagSettlement   = 0x02
	pushTagPeerExchange = 0x03
)

// runPushAcceptor accepts server-initiated streams and dispatches to
// the offer, settlement, or peer-exchange handler based on a 1-byte
// class tag. Lifetime: this goroutine exits when conn drops, ctx is
// cancelled, or AcceptStream returns a non-EOF error.
func runPushAcceptor(ctx context.Context, conn transport.Conn, off OfferHandler, set SettlementHandler, pex PeerExchangeHandler, maxFrameSize int) {
	for {
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		go dispatchPush(ctx, stream, off, set, pex, maxFrameSize)
	}
}

func dispatchPush(ctx context.Context, stream transport.Stream, off OfferHandler, set SettlementHandler, pex PeerExchangeHandler, maxFrameSize int) {
	defer stream.Close()
	var tag [1]byte
	if _, err := stream.Read(tag[:]); err != nil {
		return
	}
	switch tag[0] {
	case pushTagOffer:
		if off == nil {
			_ = wire.Write(stream, &tbproto.OfferDecision{
				Accept: false, RejectReason: ErrNoHandler.Error(),
			}, maxFrameSize)
			return
		}
		handleOffer(ctx, stream, off, maxFrameSize)
	case pushTagSettlement:
		if set == nil {
			return
		}
		handleSettlement(ctx, stream, set, maxFrameSize)
	case pushTagPeerExchange:
		// nil handler: drop silently — by-design backward compat.
		handlePeerExchange(ctx, stream, pex, maxFrameSize)
	default:
		return
	}
}

func handleSettlement(ctx context.Context, stream transport.Stream, h SettlementHandler, maxFrameSize int) {
	var push tbproto.SettlementPush
	if err := wire.Read(stream, &push, maxFrameSize); err != nil {
		return
	}
	if err := tbproto.ValidateSettlementPush(&push); err != nil {
		return
	}
	var hash [32]byte
	copy(hash[:], push.PreimageHash)
	sig, err := h.HandleSettlement(ctx, &SettlementRequest{PreimageHash: hash, PreimageBody: push.PreimageBody})
	if err != nil || sig == nil {
		return // tracker treats no-ack as deferred consent
	}
	_ = wire.Write(stream, &tbproto.SettleAck{}, maxFrameSize)
	// The actual signature flows through the Settle RPC method; this
	// ack confirms the consumer received the push and intends to settle.
	_ = sig
}

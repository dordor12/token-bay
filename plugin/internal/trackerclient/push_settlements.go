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
	if h == nil {
		return // no handler configured: drop silently, push reads completed
	}
	var hash [32]byte
	copy(hash[:], push.PreimageHash)
	if err := h.HandleSettlement(ctx, &SettlementRequest{PreimageHash: hash, PreimageBody: push.PreimageBody}); err != nil {
		return // refusal: no SettleAck — tracker records a dispute on timeout
	}
	// The countersig itself flows back through the unary Settle RPC the
	// handler invokes; this ack only confirms receipt + intent.
	_ = wire.Write(stream, &tbproto.SettleAck{}, maxFrameSize)
}

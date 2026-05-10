package trackerclient

import (
	"context"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/wire"
	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
)

// handlePeerExchange reads a single federation.Envelope from stream,
// validates it carries a well-formed PeerExchange payload, and invokes
// the configured handler. The tracker push runs on an already-mTLS-
// authenticated connection, so envelope-level signature verification is
// intentionally skipped here — the connection is the trust root.
//
// nil handler causes the function to silently consume and drop the
// frame for backward compat with deployments that don't wire a store.
func handlePeerExchange(ctx context.Context, stream transport.Stream, h PeerExchangeHandler, maxFrameSize int) {
	var env fed.Envelope
	if err := wire.Read(stream, &env, maxFrameSize); err != nil {
		return
	}
	if err := fed.ValidateEnvelope(&env); err != nil {
		return
	}
	if env.Kind != fed.Kind_KIND_PEER_EXCHANGE {
		return
	}
	var msg fed.PeerExchange
	if err := proto.Unmarshal(env.Payload, &msg); err != nil {
		return
	}
	if err := fed.ValidatePeerExchange(&msg); err != nil {
		return
	}
	if h == nil {
		return
	}

	peers := make([]BootstrapPeer, 0, len(msg.Peers))
	for _, p := range msg.Peers {
		var tid ids.IdentityID
		copy(tid[:], p.TrackerId)
		peers = append(peers, BootstrapPeer{
			TrackerID:   tid,
			Addr:        p.Addr,
			RegionHint:  p.RegionHint,
			HealthScore: p.HealthScore,
			LastSeen:    time.Unix(int64(p.LastSeen), 0), //nolint:gosec
		})
	}
	_ = h.HandlePeerExchange(ctx, peers)
}

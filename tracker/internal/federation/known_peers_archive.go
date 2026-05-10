package federation

import (
	"context"

	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

// KnownPeersArchive is the slice of *storage.Store federation needs for
// peer-exchange gossip and slice-5 peer-health write-through.
// *storage.Store satisfies it via Go structural typing; tests pass a
// fake. May be nil in Deps; when nil, peer-exchange is disabled
// (Federation.PublishPeerExchange returns ErrPeerExchangeDisabled and
// inbound KIND_PEER_EXCHANGE is rejected with metric reason
// "peer_exchange_disabled").
type KnownPeersArchive interface {
	UpsertKnownPeer(ctx context.Context, p storage.KnownPeer) error
	GetKnownPeer(ctx context.Context, trackerID []byte) (storage.KnownPeer, bool, error)
	ListKnownPeers(ctx context.Context, limit int, byHealthDesc bool) ([]storage.KnownPeer, error)
	UpdateKnownPeerHealth(ctx context.Context, trackerID []byte, score float64) error
}

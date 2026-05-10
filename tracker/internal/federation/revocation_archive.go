package federation

import (
	"context"

	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

// PeerRevocationArchive is the slice of *storage.Store the federation
// revocation path needs. Backed by *ledger/storage.Store in production;
// tests substitute an in-memory fake.
type PeerRevocationArchive interface {
	PutPeerRevocation(ctx context.Context, r storage.PeerRevocation) error
	GetPeerRevocation(ctx context.Context, trackerID, identityID []byte) (storage.PeerRevocation, bool, error)
}

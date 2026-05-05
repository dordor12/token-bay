package trackerclient

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

type balanceCache struct {
	ttlHeadroom time.Duration
	clock       func() time.Time

	mu    sync.RWMutex
	snaps map[ids.IdentityID]*tbproto.SignedBalanceSnapshot

	sf singleflight.Group
}

func newBalanceCache(headroom time.Duration, clock func() time.Time) *balanceCache {
	return &balanceCache{
		ttlHeadroom: headroom,
		clock:       clock,
		snaps:       map[ids.IdentityID]*tbproto.SignedBalanceSnapshot{},
	}
}

// fetchFn is supplied by the Client and calls Balance() under the hood.
type fetchFn func(ctx context.Context, id ids.IdentityID) (*tbproto.SignedBalanceSnapshot, error)

// Get returns a fresh-enough snapshot, fetching via fn if missing or
// within ttlHeadroom of expiry.
func (b *balanceCache) Get(ctx context.Context, id ids.IdentityID, fn fetchFn) (*tbproto.SignedBalanceSnapshot, error) {
	if snap := b.lookupFresh(id); snap != nil {
		return snap, nil
	}

	v, err, _ := b.sf.Do(string(id[:]), func() (any, error) {
		// Re-check under singleflight in case a concurrent caller refreshed.
		if snap := b.lookupFresh(id); snap != nil {
			return snap, nil
		}
		snap, err := fn(ctx, id)
		if err != nil {
			return nil, err
		}
		b.store(id, snap)
		return snap, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*tbproto.SignedBalanceSnapshot), nil
}

func (b *balanceCache) lookupFresh(id ids.IdentityID) *tbproto.SignedBalanceSnapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()
	snap, ok := b.snaps[id]
	if !ok || snap.Body == nil {
		return nil
	}
	//nolint:gosec // ExpiresAt is a unix-second timestamp; fits in int64 for any practical lifetime
	expiresAt := time.Unix(int64(snap.Body.ExpiresAt), 0)
	if b.clock().Add(b.ttlHeadroom).After(expiresAt) {
		return nil
	}
	return snap
}

func (b *balanceCache) store(id ids.IdentityID, snap *tbproto.SignedBalanceSnapshot) {
	b.mu.Lock()
	b.snaps[id] = snap
	b.mu.Unlock()
}

// invalidate clears all cached snapshots; called on connection drop.
//
//nolint:unused // wired into supervisor invalidation in a future task
func (b *balanceCache) invalidate() {
	b.mu.Lock()
	b.snaps = map[ids.IdentityID]*tbproto.SignedBalanceSnapshot{}
	b.mu.Unlock()
}

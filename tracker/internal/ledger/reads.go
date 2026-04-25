package ledger

import (
	"context"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// Tip returns the current chain tip's seq and hash, or ok=false on an
// empty chain. Thin passthrough to storage — no lock, no verification.
func (l *Ledger) Tip(ctx context.Context) (uint64, []byte, bool, error) {
	return l.store.Tip(ctx)
}

// EntryBySeq returns the entry at the given seq, or ok=false on miss.
// Returned entries are not re-validated; AssertChainIntegrity (audit-only)
// is the explicit hook for re-checking prev_hash linkage.
func (l *Ledger) EntryBySeq(ctx context.Context, seq uint64) (*tbproto.Entry, bool, error) {
	return l.store.EntryBySeq(ctx, seq)
}

// EntryByHash returns the entry with the given canonical hash, or
// ok=false on miss.
func (l *Ledger) EntryByHash(ctx context.Context, hash []byte) (*tbproto.Entry, bool, error) {
	return l.store.EntryByHash(ctx, hash)
}

// EntriesSince returns up to limit entries with seq strictly greater than
// sinceSeq, ordered ascending. limit=0 means no cap. Used by federation
// gossip + audit replay.
func (l *Ledger) EntriesSince(ctx context.Context, sinceSeq uint64, limit int) ([]*tbproto.Entry, error) {
	return l.store.EntriesSince(ctx, sinceSeq, limit)
}

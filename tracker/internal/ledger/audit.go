package ledger

import (
	"bytes"
	"context"
	"fmt"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/ledger/entry"
)

// auditBatchSize is the page size for AssertChainIntegrity — bounded so
// a long audit doesn't pin a SQLite snapshot for the whole walk.
const auditBatchSize = 1000

// AssertChainIntegrity walks entries with seq in (sinceSeq, untilSeq],
// verifying, for each entry:
//
//  1. Linkage: prev_hash equals the previous entry's body hash.
//  2. Content: the body's recomputed hash equals the hash committed at
//     append time (the row's stored hash column).
//
// The content check is what covers the tip. Linkage alone verifies an
// entry's content only through its successor's prev_hash — and the tip
// has no successor, so a corrupted tip body would pass a linkage-only
// walk undetected.
//
// Returns nil on an intact chain; an error describing the first break
// or corruption otherwise.
//
// untilSeq=0 means "up to current tip". Used for ad-hoc CI audits and
// operator tooling; not called on every read.
func (l *Ledger) AssertChainIntegrity(ctx context.Context, sinceSeq, untilSeq uint64) error {
	// Establish the "previous entry hash" anchor first. A missing anchor is
	// always an error, regardless of chain state — the caller asked us to
	// start from a specific point and the point doesn't exist.
	var prevHash []byte
	if sinceSeq == 0 {
		prevHash = make([]byte, 32)
	} else {
		anchor, ok, err := l.store.EntryBySeq(ctx, sinceSeq)
		if err != nil {
			return fmt.Errorf("ledger: AssertChainIntegrity anchor: %w", err)
		}
		if !ok {
			return fmt.Errorf("ledger: AssertChainIntegrity: anchor seq=%d missing", sinceSeq)
		}
		h, err := entry.Hash(anchor.Body)
		if err != nil {
			return fmt.Errorf("ledger: AssertChainIntegrity anchor hash: %w", err)
		}
		prevHash = h[:]
	}

	if untilSeq == 0 {
		tipSeq, _, hasTip, err := l.store.Tip(ctx)
		if err != nil {
			return fmt.Errorf("ledger: AssertChainIntegrity tip: %w", err)
		}
		if !hasTip {
			return nil // empty chain trivially intact
		}
		untilSeq = tipSeq
	}

	cursor := sinceSeq
	for cursor < untilSeq {
		batch, err := l.store.EntriesWithHashSince(ctx, cursor, auditBatchSize)
		if err != nil {
			return fmt.Errorf("ledger: AssertChainIntegrity page: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		for _, e := range batch {
			body := e.Entry.Body
			if body.Seq > untilSeq {
				return nil
			}
			if !bytes.Equal(body.PrevHash, prevHash) {
				return fmt.Errorf(
					"ledger: chain break at seq=%d: prev_hash=%x, expected=%x",
					body.Seq, body.PrevHash, prevHash,
				)
			}
			h, err := entry.Hash(body)
			if err != nil {
				return fmt.Errorf("ledger: AssertChainIntegrity hash seq=%d: %w", body.Seq, err)
			}
			// Content check: the recomputed body hash must equal the hash
			// committed at append time. This is the only check that covers
			// the tip — no successor's prev_hash ever vouches for it.
			if !bytes.Equal(h[:], e.StoredHash) {
				return fmt.Errorf(
					"ledger: content corruption at seq=%d: body hash=%x, stored hash=%x",
					body.Seq, h, e.StoredHash,
				)
			}
			prevHash = h[:]
			cursor = body.Seq
		}
	}
	return nil
}

// _ silence unused-type warning; keeps tbproto import meaningful when
// future audit checks (e.g., signature verification) inspect entry kinds.
var _ tbproto.EntryKind

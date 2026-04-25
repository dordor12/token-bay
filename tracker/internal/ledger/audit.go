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
// verifying each entry's prev_hash equals the previous entry's body hash.
// Returns nil on a chain whose entries link correctly; an error
// describing the first break otherwise.
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
		batch, err := l.store.EntriesSince(ctx, cursor, auditBatchSize)
		if err != nil {
			return fmt.Errorf("ledger: AssertChainIntegrity page: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		for _, e := range batch {
			if e.Body.Seq > untilSeq {
				return nil
			}
			if !bytes.Equal(e.Body.PrevHash, prevHash) {
				return fmt.Errorf(
					"ledger: chain break at seq=%d: prev_hash=%x, expected=%x",
					e.Body.Seq, e.Body.PrevHash, prevHash,
				)
			}
			h, err := entry.Hash(e.Body)
			if err != nil {
				return fmt.Errorf("ledger: AssertChainIntegrity hash seq=%d: %w", e.Body.Seq, err)
			}
			prevHash = h[:]
			cursor = e.Body.Seq
		}
	}
	return nil
}

// _ silence unused-type warning; keeps tbproto import meaningful when
// future audit checks (e.g., signature verification) inspect entry kinds.
var _ tbproto.EntryKind

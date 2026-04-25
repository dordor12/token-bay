package ledger

import (
	"context"
	"errors"
	"fmt"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/ledger/entry"
)

// IssueStarterGrant credits identityID with amount credits, signed by the
// tracker only. STARTER_GRANT entries have no counterparty signatures.
//
// Constructs the body inside Ledger.mu via appendEntryWithBuilder so the
// (prev_hash, seq) come from a fresh tip read in the same critical
// section as the append. No stale-tip race possible.
func (l *Ledger) IssueStarterGrant(ctx context.Context, identityID []byte, amount uint64) (*tbproto.Entry, error) {
	if amount == 0 {
		return nil, errors.New("ledger: starter grant amount must be > 0")
	}
	if len(identityID) != 32 {
		return nil, fmt.Errorf("ledger: identity_id length %d, want 32", len(identityID))
	}
	delta, err := signedAmount(amount)
	if err != nil {
		return nil, err
	}

	return l.appendEntryWithBuilder(ctx, func(prev []byte, seq, now uint64) (appendInput, error) {
		body, err := entry.BuildStarterGrantEntry(entry.StarterGrantInput{
			PrevHash:   prev,
			Seq:        seq,
			ConsumerID: identityID,
			Amount:     amount,
			Timestamp:  now,
		})
		if err != nil {
			return appendInput{}, fmt.Errorf("ledger: build starter_grant: %w", err)
		}
		return appendInput{
			body:   body,
			deltas: []balanceDelta{{identityID: identityID, delta: delta}},
		}, nil
	})
}

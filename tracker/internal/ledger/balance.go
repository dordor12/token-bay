package ledger

import (
	"context"
	"fmt"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)

// balanceSnapshotTTLSeconds is the freshness window for SignedBalanceSnapshot
// (ledger spec §3.4). Beyond this, consumers must refresh.
const balanceSnapshotTTLSeconds = 600

// SignedBalance returns the tracker-signed balance snapshot for identityID.
// The snapshot is internally consistent — credits and chain_tip_seq reflect
// the same point in chain history (storage.BalanceWithTip is one read
// transaction).
//
// Identities with no entries return credits=0; chain_tip_hash is 32 zero
// bytes when the chain is empty. expires_at is issued_at + 600.
func (l *Ledger) SignedBalance(ctx context.Context, identityID []byte) (*tbproto.SignedBalanceSnapshot, error) {
	if len(identityID) != 32 {
		return nil, fmt.Errorf("ledger: identity_id length %d, want 32", len(identityID))
	}

	snap, err := l.store.BalanceWithTip(ctx, identityID)
	if err != nil {
		return nil, fmt.Errorf("ledger: BalanceWithTip: %w", err)
	}

	tipHash := snap.TipHash
	if !snap.TipFound {
		tipHash = make([]byte, 32)
	}

	issued := uint64(l.nowFn().Unix())
	body := &tbproto.BalanceSnapshotBody{
		IdentityId:   identityID,
		Credits:      snap.Balance.Credits, // 0 if !BalanceFound
		ChainTipHash: tipHash,
		ChainTipSeq:  snap.TipSeq, // 0 if !TipFound
		IssuedAt:     issued,
		ExpiresAt:    issued + balanceSnapshotTTLSeconds,
	}

	sig, err := signing.SignBalanceSnapshot(l.trackerKey, body)
	if err != nil {
		return nil, fmt.Errorf("ledger: sign snapshot: %w", err)
	}
	return &tbproto.SignedBalanceSnapshot{Body: body, TrackerSig: sig}, nil
}

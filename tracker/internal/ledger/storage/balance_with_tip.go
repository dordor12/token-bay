package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// BalanceTipSnapshot is a single-transaction view of an identity's balance
// row and the current chain tip. ledger.SignedBalance uses this so the
// signed snapshot's chain_tip_seq and credits reflect the same point in
// chain history (ledger spec §4.2).
type BalanceTipSnapshot struct {
	Balance      BalanceRow
	BalanceFound bool   // false if the identity has no row yet
	TipSeq       uint64 // 0 if !TipFound
	TipHash      []byte // nil if !TipFound
	TipFound     bool   // false on empty chain
}

// BalanceWithTip reads identityID's balance row and the current chain tip
// in a single SQLite read transaction. Either may be missing — the caller
// inspects BalanceFound / TipFound. Errors only on actual DB problems.
func (s *Store) BalanceWithTip(ctx context.Context, identityID []byte) (BalanceTipSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return BalanceTipSnapshot{}, fmt.Errorf("storage: BalanceWithTip begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var snap BalanceTipSnapshot

	bRow := tx.QueryRowContext(ctx,
		`SELECT identity_id, credits, last_seq, updated_at FROM balances WHERE identity_id = ?`, identityID)
	bErr := bRow.Scan(&snap.Balance.IdentityID, &snap.Balance.Credits, &snap.Balance.LastSeq, &snap.Balance.UpdatedAt)
	switch {
	case errors.Is(bErr, sql.ErrNoRows):
		// BalanceFound stays false.
	case bErr != nil:
		return BalanceTipSnapshot{}, fmt.Errorf("storage: BalanceWithTip balance: %w", bErr)
	default:
		snap.BalanceFound = true
	}

	tRow := tx.QueryRowContext(ctx, `SELECT seq, hash FROM entries ORDER BY seq DESC LIMIT 1`)
	tErr := tRow.Scan(&snap.TipSeq, &snap.TipHash)
	switch {
	case errors.Is(tErr, sql.ErrNoRows):
		// TipFound stays false.
	case tErr != nil:
		return BalanceTipSnapshot{}, fmt.Errorf("storage: BalanceWithTip tip: %w", tErr)
	default:
		snap.TipFound = true
	}

	if err := tx.Commit(); err != nil {
		return BalanceTipSnapshot{}, fmt.Errorf("storage: BalanceWithTip commit: %w", err)
	}
	return snap, nil
}

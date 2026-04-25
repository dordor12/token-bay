package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// BalanceRow mirrors the balances table.
type BalanceRow struct {
	IdentityID []byte
	Credits    int64
	LastSeq    uint64
	UpdatedAt  uint64
}

// EntryBySeq returns the entry at the given seq, or ok=false on a clean
// miss. The wire-form *tbproto.Entry is reconstructed from the row's
// canonical blob — the indexed columns are projections, never read here.
func (s *Store) EntryBySeq(ctx context.Context, seq uint64) (*tbproto.Entry, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT canonical, consumer_sig, seeder_sig, tracker_sig FROM entries WHERE seq = ?`, seq)
	return scanEntry(row)
}

// EntryByHash returns the entry with the given canonical hash, or
// ok=false on a clean miss.
func (s *Store) EntryByHash(ctx context.Context, hash []byte) (*tbproto.Entry, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT canonical, consumer_sig, seeder_sig, tracker_sig FROM entries WHERE hash = ?`, hash)
	return scanEntry(row)
}

// Balance returns the current balance projection for an identity, or
// ok=false on a clean miss.
func (s *Store) Balance(ctx context.Context, identityID []byte) (BalanceRow, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT identity_id, credits, last_seq, updated_at FROM balances WHERE identity_id = ?`, identityID)
	var b BalanceRow
	err := row.Scan(&b.IdentityID, &b.Credits, &b.LastSeq, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return BalanceRow{}, false, nil
	}
	if err != nil {
		return BalanceRow{}, false, fmt.Errorf("storage: Balance: %w", err)
	}
	return b, true, nil
}

// scanEntry parses one row's canonical+sigs into a *tbproto.Entry. Used by
// EntryBySeq and EntryByHash so the canonical-bytes round-trip stays the
// single source of truth.
func scanEntry(row *sql.Row) (*tbproto.Entry, bool, error) {
	var (
		canonical   []byte
		consumerSig []byte
		seederSig   []byte
		trackerSig  []byte
	)
	err := row.Scan(&canonical, &consumerSig, &seederSig, &trackerSig)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("storage: scan entry: %w", err)
	}
	body := &tbproto.EntryBody{}
	if err := proto.Unmarshal(canonical, body); err != nil {
		return nil, false, fmt.Errorf("storage: unmarshal canonical: %w", err)
	}
	return &tbproto.Entry{
		Body:        body,
		ConsumerSig: consumerSig,
		SeederSig:   seederSig,
		TrackerSig:  trackerSig,
	}, true, nil
}

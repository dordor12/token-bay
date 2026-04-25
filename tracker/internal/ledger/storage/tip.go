package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Tip returns the seq and hash of the highest-numbered entry in the chain.
// Returns ok=false (and zero values) if the chain is empty. The orchestrator
// uses this to compute the next entry's prev_hash.
func (s *Store) Tip(ctx context.Context) (seq uint64, hash []byte, ok bool, err error) {
	row := s.db.QueryRowContext(ctx, `SELECT seq, hash FROM entries ORDER BY seq DESC LIMIT 1`)
	var rawHash []byte
	err = row.Scan(&seq, &rawHash)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, false, nil
	}
	if err != nil {
		return 0, nil, false, fmt.Errorf("storage: Tip: %w", err)
	}
	return seq, rawHash, true, nil
}

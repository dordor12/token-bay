package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrDuplicateMerkleRoot signals that a Merkle root for the given hour
// already exists. Returned by PutMerkleRoot when attempting to overwrite
// an already-published hour. Idempotent re-publication of the *same*
// (root, sig) is not allowed either — the orchestrator should not be
// publishing the same hour twice.
var ErrDuplicateMerkleRoot = errors.New("storage: merkle root for this hour already exists")

// hourSeconds is the duration of one Merkle-root bucket.
const hourSeconds = uint64(3600)

// LeavesForHour returns the entry hashes whose timestamp falls in
// [hour*3600, (hour+1)*3600), ordered by seq ascending. The orchestrator
// computes the Merkle root over these leaves.
func (s *Store) LeavesForHour(ctx context.Context, hour uint64) ([][]byte, error) {
	start := hour * hourSeconds
	end := start + hourSeconds

	rows, err := s.db.QueryContext(ctx,
		`SELECT hash FROM entries WHERE timestamp >= ? AND timestamp < ? ORDER BY seq ASC`,
		start, end,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: LeavesForHour query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out [][]byte
	for rows.Next() {
		var h []byte
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("storage: LeavesForHour scan: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: LeavesForHour rows: %w", err)
	}
	return out, nil
}

// PutMerkleRoot persists an hourly Merkle root + tracker signature. Returns
// ErrDuplicateMerkleRoot if a root for this hour already exists, even if
// the row matches byte-for-byte — re-publishing the same hour is a
// programmer error in v1.
func (s *Store) PutMerkleRoot(ctx context.Context, hour uint64, root []byte, trackerSig []byte) error {
	if len(root) == 0 || len(trackerSig) == 0 {
		return errors.New("storage: PutMerkleRoot requires non-empty root and trackerSig")
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO merkle_roots (hour, root, tracker_sig) VALUES (?,?,?)`,
		hour, root, trackerSig,
	)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "PRIMARY KEY") || strings.Contains(msg, "UNIQUE constraint failed: merkle_roots.hour") {
			return ErrDuplicateMerkleRoot
		}
		return fmt.Errorf("storage: PutMerkleRoot: %w", err)
	}
	return nil
}

// GetMerkleRoot returns the persisted root + tracker_sig for an hour, or
// ok=false on miss.
func (s *Store) GetMerkleRoot(ctx context.Context, hour uint64) (root, sig []byte, ok bool, err error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT root, tracker_sig FROM merkle_roots WHERE hour = ?`, hour)
	err = row.Scan(&root, &sig)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("storage: GetMerkleRoot: %w", err)
	}
	return root, sig, true, nil
}

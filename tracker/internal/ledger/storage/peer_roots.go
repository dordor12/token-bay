package storage

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PeerRoot is one (tracker_id, hour) row from peer_root_archive.
type PeerRoot struct {
	TrackerID  []byte
	Hour       uint64
	Root       []byte
	Sig        []byte
	ReceivedAt uint64
}

// ErrPeerRootConflict is returned when PutPeerRoot tries to write a row
// for a (tracker_id, hour) that already has a different (root, sig).
// The wrapping message includes both the existing and incoming bytes so
// the equivocation-detection layer (out of scope here) can act on it.
//
// An identical-row Put returns nil — peers re-broadcasting the same
// gossip via different routes is normal, not equivocation.
var ErrPeerRootConflict = errors.New("storage: peer root conflict")

// PutPeerRoot persists a peer's published Merkle root for an hour.
// Idempotent on identical rows (same tracker_id, hour, root, sig);
// returns ErrPeerRootConflict on a differing row for the same key.
func (s *Store) PutPeerRoot(ctx context.Context, p PeerRoot) error {
	if len(p.TrackerID) == 0 || len(p.Root) == 0 || len(p.Sig) == 0 {
		return errors.New("storage: PutPeerRoot requires non-empty tracker_id, root, sig")
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	// Check for an existing row first; if found, decide between idempotent
	// no-op and ErrPeerRootConflict. Doing this inside the write lock means
	// we don't race a concurrent put for the same (tracker_id, hour).
	row := s.db.QueryRowContext(ctx,
		`SELECT root, sig FROM peer_root_archive WHERE tracker_id = ? AND hour = ?`,
		p.TrackerID, p.Hour,
	)
	var existingRoot, existingSig []byte
	err := row.Scan(&existingRoot, &existingSig)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No conflict; insert below.
	case err != nil:
		return fmt.Errorf("storage: PutPeerRoot existence check: %w", err)
	default:
		if bytes.Equal(existingRoot, p.Root) && bytes.Equal(existingSig, p.Sig) {
			return nil // idempotent no-op
		}
		return fmt.Errorf("%w: tracker_id=%x hour=%d existing_root=%x incoming_root=%x",
			ErrPeerRootConflict, p.TrackerID, p.Hour, existingRoot, p.Root)
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO peer_root_archive (tracker_id, hour, root, sig, received_at) VALUES (?,?,?,?,?)`,
		p.TrackerID, p.Hour, p.Root, p.Sig, p.ReceivedAt,
	); err != nil {
		return fmt.Errorf("storage: PutPeerRoot insert: %w", err)
	}
	return nil
}

// GetPeerRoot returns the row for (tracker_id, hour), or ok=false on miss.
func (s *Store) GetPeerRoot(ctx context.Context, trackerID []byte, hour uint64) (PeerRoot, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT tracker_id, hour, root, sig, received_at FROM peer_root_archive WHERE tracker_id = ? AND hour = ?`,
		trackerID, hour,
	)
	var p PeerRoot
	err := row.Scan(&p.TrackerID, &p.Hour, &p.Root, &p.Sig, &p.ReceivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PeerRoot{}, false, nil
	}
	if err != nil {
		return PeerRoot{}, false, fmt.Errorf("storage: GetPeerRoot: %w", err)
	}
	return p, true, nil
}

// ListPeerRootsForHour returns all peers' published roots for an hour,
// ordered by tracker_id ascending. Used when reconciling federation gossip
// or computing equivocation evidence across peers.
func (s *Store) ListPeerRootsForHour(ctx context.Context, hour uint64) ([]PeerRoot, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tracker_id, hour, root, sig, received_at FROM peer_root_archive WHERE hour = ? ORDER BY tracker_id ASC`,
		hour,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: ListPeerRootsForHour query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []PeerRoot
	for rows.Next() {
		var p PeerRoot
		if err := rows.Scan(&p.TrackerID, &p.Hour, &p.Root, &p.Sig, &p.ReceivedAt); err != nil {
			return nil, fmt.Errorf("storage: ListPeerRootsForHour scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: ListPeerRootsForHour rows: %w", err)
	}
	return out, nil
}

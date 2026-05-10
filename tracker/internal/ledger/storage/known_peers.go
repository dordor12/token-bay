package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// KnownPeer is one row from known_peers. Source is "allowlist" for
// operator-configured peers and "gossip" for entries learned via
// PEER_EXCHANGE.
type KnownPeer struct {
	TrackerID   []byte
	Addr        string
	LastSeen    time.Time
	RegionHint  string
	HealthScore float64
	Source      string
}

// UpsertKnownPeer inserts or updates one known_peers row.
//
// SQL invariants enforced in this statement:
//   - Last-write-wins by last_seen: a stale upsert (excluded.last_seen <
//     known_peers.last_seen) is a no-op for an existing 'gossip' row.
//   - Allowlist rows are pinned: addr and region_hint of an 'allowlist'
//     row cannot be mutated by a 'gossip' write. health_score and
//     last_seen of an 'allowlist' row may still update so slice-5
//     metrics can flow in.
//   - source is never changed by an upsert.
func (s *Store) UpsertKnownPeer(ctx context.Context, p KnownPeer) error {
	if len(p.TrackerID) == 0 {
		return errors.New("storage: UpsertKnownPeer requires non-empty tracker_id")
	}
	if p.Addr == "" {
		return errors.New("storage: UpsertKnownPeer requires non-empty addr")
	}
	if p.Source != "allowlist" && p.Source != "gossip" {
		return fmt.Errorf("storage: UpsertKnownPeer invalid source %q", p.Source)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO known_peers (tracker_id, addr, last_seen, region_hint, health_score, source)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(tracker_id) DO UPDATE SET
		    addr         = CASE WHEN known_peers.source = 'allowlist' AND excluded.source = 'gossip'
		                        THEN known_peers.addr ELSE excluded.addr END,
		    last_seen    = MAX(known_peers.last_seen, excluded.last_seen),
		    region_hint  = CASE WHEN known_peers.source = 'allowlist' AND excluded.source = 'gossip'
		                        THEN known_peers.region_hint ELSE excluded.region_hint END,
		    health_score = CASE WHEN excluded.last_seen >= known_peers.last_seen
		                        THEN excluded.health_score ELSE known_peers.health_score END,
		    source       = known_peers.source
		WHERE excluded.last_seen >= known_peers.last_seen
		   OR known_peers.source  = 'allowlist'`,
		p.TrackerID, p.Addr, p.LastSeen.Unix(), p.RegionHint, p.HealthScore, p.Source,
	); err != nil {
		return fmt.Errorf("storage: UpsertKnownPeer: %w", err)
	}
	return nil
}

// GetKnownPeer returns the row for trackerID, or ok=false on miss.
func (s *Store) GetKnownPeer(ctx context.Context, trackerID []byte) (KnownPeer, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT tracker_id, addr, last_seen, region_hint, health_score, source
		FROM known_peers WHERE tracker_id = ?`,
		trackerID,
	)
	var (
		p        KnownPeer
		lastSeen int64
	)
	err := row.Scan(&p.TrackerID, &p.Addr, &lastSeen, &p.RegionHint, &p.HealthScore, &p.Source)
	if errors.Is(err, sql.ErrNoRows) {
		return KnownPeer{}, false, nil
	}
	if err != nil {
		return KnownPeer{}, false, fmt.Errorf("storage: GetKnownPeer: %w", err)
	}
	p.LastSeen = time.Unix(lastSeen, 0)
	return p, true, nil
}

// ListKnownPeers returns up to limit rows, optionally sorted by health
// descending (then tracker_id ascending for deterministic tiebreak).
func (s *Store) ListKnownPeers(ctx context.Context, limit int, byHealthDesc bool) ([]KnownPeer, error) {
	if limit <= 0 {
		return nil, nil
	}
	const qByHealth = `
		SELECT tracker_id, addr, last_seen, region_hint, health_score, source
		FROM known_peers
		ORDER BY health_score DESC, tracker_id ASC
		LIMIT ?`
	const qByID = `
		SELECT tracker_id, addr, last_seen, region_hint, health_score, source
		FROM known_peers
		ORDER BY tracker_id ASC
		LIMIT ?`
	q := qByID
	if byHealthDesc {
		q = qByHealth
	}
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("storage: ListKnownPeers: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []KnownPeer
	for rows.Next() {
		var (
			p        KnownPeer
			lastSeen int64
		)
		if err := rows.Scan(&p.TrackerID, &p.Addr, &lastSeen, &p.RegionHint, &p.HealthScore, &p.Source); err != nil {
			return nil, fmt.Errorf("storage: ListKnownPeers scan: %w", err)
		}
		p.LastSeen = time.Unix(lastSeen, 0)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: ListKnownPeers rows: %w", err)
	}
	return out, nil
}

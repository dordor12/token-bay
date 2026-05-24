package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PeerRevocation is one row from peer_revocations.
type PeerRevocation struct {
	TrackerID  []byte // 32 — issuer
	IdentityID []byte // 32 — revoked identity
	Reason     uint32 // RevocationReason enum
	RevokedAt  uint64 // unix seconds (issuer's clock)
	TrackerSig []byte // 64 bytes
	ReceivedAt uint64 // unix seconds (local clock)
}

// PutPeerRevocation persists r idempotently. INSERT OR IGNORE on the
// composite (tracker_id, identity_id) primary key: a duplicate revocation
// from gossip-echoed paths is a silent no-op, preserving the first writer.
func (s *Store) PutPeerRevocation(ctx context.Context, r PeerRevocation) error {
	if len(r.TrackerID) == 0 || len(r.IdentityID) == 0 || len(r.TrackerSig) == 0 {
		return errors.New("storage: PutPeerRevocation requires non-empty tracker_id, identity_id, tracker_sig")
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if _, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO peer_revocations
		    (tracker_id, identity_id, reason, revoked_at, tracker_sig, received_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		r.TrackerID, r.IdentityID, r.Reason, r.RevokedAt, r.TrackerSig, r.ReceivedAt,
	); err != nil {
		return fmt.Errorf("storage: PutPeerRevocation insert: %w", err)
	}
	return nil
}

// IsIdentityRevoked reports whether any peer tracker has gossiped a
// REVOCATION for identityID. The broker pre-check uses this to refuse
// a broker_request from an identity FROZEN at any peer regardless of
// the originating region — see federation §6.1, reputation §12.
func (s *Store) IsIdentityRevoked(ctx context.Context, identityID []byte) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM peer_revocations WHERE identity_id = ? LIMIT 1`,
		identityID,
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("storage: IsIdentityRevoked: %w", err)
	}
	return true, nil
}

// ListRevocationsForIdentity returns every peer_revocations row whose
// identity_id matches, across all issuers. Used by the registry +
// admission integration (slice 12) to enforce §6 third bullet: tear
// down active sessions on revocation, refuse subsequent enrolls.
func (s *Store) ListRevocationsForIdentity(ctx context.Context, identityID []byte) ([]PeerRevocation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT tracker_id, identity_id, reason, revoked_at, tracker_sig, received_at
		FROM peer_revocations WHERE identity_id = ?
		ORDER BY received_at ASC, tracker_id ASC`,
		identityID,
	)
	if err != nil {
		return nil, fmt.Errorf("storage: ListRevocationsForIdentity: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []PeerRevocation
	for rows.Next() {
		var r PeerRevocation
		if err := rows.Scan(&r.TrackerID, &r.IdentityID, &r.Reason, &r.RevokedAt, &r.TrackerSig, &r.ReceivedAt); err != nil {
			return nil, fmt.Errorf("storage: ListRevocationsForIdentity scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: ListRevocationsForIdentity rows: %w", err)
	}
	return out, nil
}

// GetPeerRevocation returns the row for (trackerID, identityID), or
// ok=false on miss.
func (s *Store) GetPeerRevocation(ctx context.Context, trackerID, identityID []byte) (PeerRevocation, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT tracker_id, identity_id, reason, revoked_at, tracker_sig, received_at
		FROM peer_revocations WHERE tracker_id = ? AND identity_id = ?`,
		trackerID, identityID,
	)
	var r PeerRevocation
	err := row.Scan(&r.TrackerID, &r.IdentityID, &r.Reason, &r.RevokedAt, &r.TrackerSig, &r.ReceivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PeerRevocation{}, false, nil
	}
	if err != nil {
		return PeerRevocation{}, false, fmt.Errorf("storage: GetPeerRevocation: %w", err)
	}
	return r, true, nil
}

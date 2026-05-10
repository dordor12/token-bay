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

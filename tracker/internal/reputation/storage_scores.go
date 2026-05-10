package reputation

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// upsertScore inserts or replaces the score for id.
func (s *storage) upsertScore(ctx context.Context, id ids.IdentityID,
	score float64, now time.Time,
) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO rep_scores (identity_id, score, updated_at)
		     VALUES (?, ?, ?)
		ON CONFLICT(identity_id) DO UPDATE SET
		     score = excluded.score,
		     updated_at = excluded.updated_at`,
		id[:], score, now.Unix())
	if err != nil {
		return fmt.Errorf("reputation: upsertScore: %w", err)
	}
	return nil
}

// loadAllScores returns every rep_scores row as a map. Used by the
// evaluator to seed the cache after a process restart.
func (s *storage) loadAllScores(ctx context.Context) (map[ids.IdentityID]float64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT identity_id, score FROM rep_scores`)
	if err != nil {
		return nil, fmt.Errorf("reputation: loadAllScores: %w", err)
	}
	defer rows.Close()

	out := map[ids.IdentityID]float64{}
	for rows.Next() {
		var (
			blob  []byte
			score float64
		)
		if err := rows.Scan(&blob, &score); err != nil {
			return nil, fmt.Errorf("reputation: loadAllScores scan: %w", err)
		}
		if len(blob) != 32 {
			return nil, fmt.Errorf("reputation: loadAllScores: identity_id length %d",
				len(blob))
		}
		var id ids.IdentityID
		copy(id[:], blob)
		out[id] = score
	}
	return out, rows.Err()
}

// loadAllStates returns every rep_state row as a map. Used by the
// evaluator to seed the cache after a process restart.
func (s *storage) loadAllStates(ctx context.Context) (map[ids.IdentityID]stateRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT identity_id, state, since, first_seen_at, reasons
		  FROM rep_state`)
	if err != nil {
		return nil, fmt.Errorf("reputation: loadAllStates: %w", err)
	}
	defer rows.Close()

	out := map[ids.IdentityID]stateRow{}
	for rows.Next() {
		var (
			blob        []byte
			state       int
			since, fs   int64
			reasonsJSON string
		)
		if err := rows.Scan(&blob, &state, &since, &fs, &reasonsJSON); err != nil {
			return nil, fmt.Errorf("reputation: loadAllStates scan: %w", err)
		}
		if len(blob) != 32 {
			return nil, fmt.Errorf("reputation: loadAllStates: identity_id length %d",
				len(blob))
		}
		var id ids.IdentityID
		copy(id[:], blob)
		var reasons []ReasonRecord
		if reasonsJSON != "" {
			if err := json.Unmarshal([]byte(reasonsJSON), &reasons); err != nil {
				return nil, fmt.Errorf("reputation: loadAllStates: decode reasons: %w", err)
			}
		}
		out[id] = stateRow{
			IdentityID:  id,
			State:       State(state), //nolint:gosec // SQLite INTEGER fits in uint8 by schema invariant
			Since:       time.Unix(since, 0),
			FirstSeenAt: time.Unix(fs, 0),
			Reasons:     reasons,
		}
	}
	return out, rows.Err()
}

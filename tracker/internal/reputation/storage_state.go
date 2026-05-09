package reputation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// errInvalidTransition is returned when transition is asked to leave
// FROZEN or to leave/enter the same state. Internal — never surfaced
// to callers; the breach mutex caller logs and metrics it.
var errInvalidTransition = errors.New("reputation: invalid state transition")

// stateRow is the in-memory shape of a rep_state row, decoded from
// SQLite + the JSON reasons column.
type stateRow struct {
	IdentityID  ids.IdentityID
	State       State
	Since       time.Time
	FirstSeenAt time.Time
	Reasons     []ReasonRecord
}

// ensureState inserts a fresh OK row for id with first_seen_at=now if
// no row exists yet. Idempotent on a second call: existing rows are
// not updated.
func (s *storage) ensureState(ctx context.Context, id ids.IdentityID, now time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `
        INSERT OR IGNORE INTO rep_state
            (identity_id, state, since, first_seen_at, reasons, updated_at)
        VALUES (?, ?, ?, ?, '[]', ?)`,
		id[:], int(StateOK), now.Unix(), now.Unix(), now.Unix())
	if err != nil {
		return fmt.Errorf("reputation: ensureState: %w", err)
	}
	return nil
}

// readState returns the current row for id. ok=false when no row exists.
func (s *storage) readState(ctx context.Context, id ids.IdentityID) (stateRow, bool, error) {
	var (
		state                   uint8
		since, firstSeen, updAt int64
		reasonsJSON             string
	)
	err := s.db.QueryRowContext(ctx, `
        SELECT state, since, first_seen_at, reasons, updated_at
          FROM rep_state WHERE identity_id = ?`, id[:]).
		Scan(&state, &since, &firstSeen, &reasonsJSON, &updAt)
	if errors.Is(err, sqlNoRows()) {
		return stateRow{}, false, nil
	}
	if err != nil {
		return stateRow{}, false, fmt.Errorf("reputation: readState: %w", err)
	}
	var reasons []ReasonRecord
	if reasonsJSON != "" {
		if err := json.Unmarshal([]byte(reasonsJSON), &reasons); err != nil {
			return stateRow{}, false,
				fmt.Errorf("reputation: readState: decode reasons: %w", err)
		}
	}
	return stateRow{
		IdentityID:  id,
		State:       State(state),
		Since:       time.Unix(since, 0),
		FirstSeenAt: time.Unix(firstSeen, 0),
		Reasons:     reasons,
	}, true, nil
}

// transition moves id to `to`, appends `reason` to the reasons array,
// and bumps since/updated_at to `now`. Returns errInvalidTransition
// when canTransition rejects the move.
//
// Caller must hold the breach mutex (or be the evaluator under its
// own serialization) — transition does not synchronize with concurrent
// readers of rep_state beyond what writeMu provides.
func (s *storage) transition(ctx context.Context, id ids.IdentityID,
	to State, reason ReasonRecord, now time.Time,
) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	cur, ok, err := s.readStateLocked(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		// Lazy ensure for callers that didn't pre-ensure.
		cur = stateRow{
			IdentityID: id, State: StateOK,
			FirstSeenAt: now, Since: now,
		}
		if _, err := s.db.ExecContext(ctx, `
            INSERT INTO rep_state
                (identity_id, state, since, first_seen_at, reasons, updated_at)
            VALUES (?, ?, ?, ?, '[]', ?)`,
			id[:], int(StateOK), now.Unix(), now.Unix(), now.Unix()); err != nil {
			return fmt.Errorf("reputation: transition: insert: %w", err)
		}
	}
	if !canTransition(cur.State, to) {
		return errInvalidTransition
	}
	cur.Reasons = append(cur.Reasons, reason)
	payload, err := json.Marshal(cur.Reasons)
	if err != nil {
		return fmt.Errorf("reputation: transition: encode reasons: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
        UPDATE rep_state
           SET state = ?, since = ?, reasons = ?, updated_at = ?
         WHERE identity_id = ?`,
		int(to), now.Unix(), string(payload), now.Unix(), id[:]); err != nil {
		return fmt.Errorf("reputation: transition: update: %w", err)
	}
	return nil
}

// appendReason adds reason to id's reasons array without changing
// state or since. Used when a categorical breach fires for an identity
// already in the breach's target state — the audit trail still needs
// the row so the evaluator can count breaches for §6.2 freeze logic.
func (s *storage) appendReason(ctx context.Context, id ids.IdentityID,
	reason ReasonRecord, now time.Time,
) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	cur, ok, err := s.readStateLocked(ctx, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("reputation: appendReason: identity not found")
	}
	cur.Reasons = append(cur.Reasons, reason)
	payload, err := json.Marshal(cur.Reasons)
	if err != nil {
		return fmt.Errorf("reputation: appendReason: encode reasons: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
        UPDATE rep_state
           SET reasons = ?, updated_at = ?
         WHERE identity_id = ?`,
		string(payload), now.Unix(), id[:]); err != nil {
		return fmt.Errorf("reputation: appendReason: update: %w", err)
	}
	return nil
}

// readStateLocked is the internal variant of readState used inside
// transition while holding writeMu. Same semantics; separate name so
// callers don't reach for the public-style readState which itself
// would re-acquire writeMu (it doesn't today, but future refactors
// might) — keeping the lock-held vs lock-free split explicit.
func (s *storage) readStateLocked(ctx context.Context, id ids.IdentityID) (stateRow, bool, error) {
	return s.readState(ctx, id)
}

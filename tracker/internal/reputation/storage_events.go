package reputation

import (
	"context"
	"fmt"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// eventAggregateRow is one identity's aggregated samples for a (signal,
// window) pair.
type eventAggregateRow struct {
	IdentityID ids.IdentityID
	Sum        float64
	Count      int64
}

// appendEvent inserts a single rep_events row.
func (s *storage) appendEvent(ctx context.Context, id ids.IdentityID,
	role Role, kind SignalKind, value float64, observedAt time.Time,
) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO rep_events (identity_id, role, event_type, value, observed_at)
		VALUES (?, ?, ?, ?, ?)`,
		id[:], int(role), int(kind), value, observedAt.Unix())
	if err != nil {
		return fmt.Errorf("reputation: appendEvent: %w", err)
	}
	return nil
}

// aggregateBySignal returns one row per identity that has at least one
// rep_events row for `kind` in [from, to). Rows are unsorted.
func (s *storage) aggregateBySignal(ctx context.Context, kind SignalKind,
	from, to time.Time,
) ([]eventAggregateRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT identity_id, SUM(value), COUNT(*)
		  FROM rep_events
		 WHERE event_type = ?
		   AND observed_at >= ? AND observed_at < ?
		 GROUP BY identity_id`,
		int(kind), from.Unix(), to.Unix())
	if err != nil {
		return nil, fmt.Errorf("reputation: aggregate: %w", err)
	}
	defer rows.Close()

	var out []eventAggregateRow
	for rows.Next() {
		var (
			blob  []byte
			sum   float64
			count int64
		)
		if err := rows.Scan(&blob, &sum, &count); err != nil {
			return nil, fmt.Errorf("reputation: aggregate scan: %w", err)
		}
		if len(blob) != 32 {
			return nil, fmt.Errorf(
				"reputation: aggregate: identity_id length %d", len(blob))
		}
		var id ids.IdentityID
		copy(id[:], blob)
		out = append(out, eventAggregateRow{
			IdentityID: id, Sum: sum, Count: count,
		})
	}
	return out, rows.Err()
}

// populationSize returns the count of distinct identities that have at
// least one rep_events row of the given role in [from, to).
func (s *storage) populationSize(ctx context.Context, role Role,
	from, to time.Time,
) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT identity_id)
		  FROM rep_events
		 WHERE role = ?
		   AND observed_at >= ? AND observed_at < ?`,
		int(role), from.Unix(), to.Unix()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("reputation: populationSize: %w", err)
	}
	return n, nil
}

// pruneEventsBefore deletes rep_events rows with observed_at < cutoff.
// Returns the number of rows deleted.
func (s *storage) pruneEventsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM rep_events WHERE observed_at < ?`, cutoff.Unix())
	if err != nil {
		return 0, fmt.Errorf("reputation: pruneEventsBefore: %w", err)
	}
	return res.RowsAffected()
}

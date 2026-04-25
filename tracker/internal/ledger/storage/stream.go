package storage

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// EntriesSince returns up to limit entries with seq strictly greater than
// sinceSeq, ordered ascending by seq. limit=0 means "no limit" — callers
// should usually pass a sensible cap (e.g., 1000) to avoid loading the
// whole chain into memory.
//
// Used by federation gossip and audit/reputation replay.
func (s *Store) EntriesSince(ctx context.Context, sinceSeq uint64, limit int) ([]*tbproto.Entry, error) {
	q := `SELECT canonical, consumer_sig, seeder_sig, tracker_sig
	      FROM entries
	      WHERE seq > ?
	      ORDER BY seq ASC`
	args := []any{sinceSeq}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("storage: EntriesSince query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*tbproto.Entry
	for rows.Next() {
		var canonical, consumerSig, seederSig, trackerSig []byte
		if err := rows.Scan(&canonical, &consumerSig, &seederSig, &trackerSig); err != nil {
			return nil, fmt.Errorf("storage: EntriesSince scan: %w", err)
		}
		body := &tbproto.EntryBody{}
		if err := proto.Unmarshal(canonical, body); err != nil {
			return nil, fmt.Errorf("storage: EntriesSince unmarshal canonical: %w", err)
		}
		out = append(out, &tbproto.Entry{
			Body:        body,
			ConsumerSig: consumerSig,
			SeederSig:   seederSig,
			TrackerSig:  trackerSig,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: EntriesSince rows: %w", err)
	}
	return out, nil
}

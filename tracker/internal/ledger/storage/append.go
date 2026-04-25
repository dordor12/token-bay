package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)

// BalanceUpdate is what the orchestrator tells storage to apply during
// AppendEntry. Storage does NOT compute these values — it persists the
// (identity_id, credits, last_seq, updated_at) row by upsert.
type BalanceUpdate struct {
	IdentityID []byte // 32 bytes
	Credits    int64
	LastSeq    uint64
	UpdatedAt  uint64
}

// AppendInput bundles the entry plus the balance projections that must
// be updated atomically with it.
type AppendInput struct {
	Entry    *tbproto.Entry  // must have Body and a tracker_sig; not validated here
	Hash     [32]byte        // entry hash; orchestrator passes the hash it already computed
	Balances []BalanceUpdate // 0..2 rows; usage entries touch 2, transfers/grants touch 1
}

// ErrAlreadyExists signals that an entry at this seq or with this hash
// already exists. The orchestrator's idempotent-retry path treats this as
// success; an orchestrator bug producing a *different* body at an existing
// seq surfaces here too, but storage cannot distinguish (SQLite reports
// the PRIMARY KEY violation first; the hash UNIQUE never gets a chance to
// fire on a same-body retry because seq is hashed into the body).
var ErrAlreadyExists = errors.New("storage: entry with this seq or hash already exists")

// AppendEntry persists one ledger entry plus its balance projections in a
// single durable transaction. Returns the entry's seq.
//
// Pre-conditions (orchestrator's responsibility, not re-checked here):
//   - in.Entry.Body.PrevHash matches the current Tip's hash (or is zero on genesis)
//   - in.Entry.Body.Seq equals current Tip seq + 1 (or 1 on genesis)
//   - in.Hash matches sha256(DeterministicMarshal(in.Entry.Body))
//   - all signatures pass entry.VerifyAll
//   - balance updates reflect the new credits after applying body.CostCredits
func (s *Store) AppendEntry(ctx context.Context, in AppendInput) (uint64, error) {
	if in.Entry == nil || in.Entry.Body == nil {
		return 0, errors.New("storage: AppendEntry requires non-nil entry + body")
	}
	if len(in.Entry.TrackerSig) == 0 {
		return 0, errors.New("storage: AppendEntry requires non-empty tracker_sig")
	}
	body := in.Entry.Body

	// Marshal the canonical bytes once. The orchestrator computed Hash from
	// these same bytes; we re-marshal here to persist them verbatim alongside
	// the indexed columns.
	canonical, err := signing.DeterministicMarshal(body)
	if err != nil {
		return 0, fmt.Errorf("storage: marshal canonical: %w", err)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("storage: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
        INSERT INTO entries (
          seq, prev_hash, hash, kind, consumer_id, seeder_id, model,
          input_tokens, output_tokens, cost_credits, timestamp,
          request_id, flags, ref, consumer_sig, seeder_sig, tracker_sig, canonical
        ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		body.Seq, body.PrevHash, in.Hash[:], int32(body.Kind),
		body.ConsumerId, body.SeederId, body.Model,
		body.InputTokens, body.OutputTokens, body.CostCredits, body.Timestamp,
		body.RequestId, body.Flags, body.Ref,
		nullableBytes(in.Entry.ConsumerSig), nullableBytes(in.Entry.SeederSig),
		in.Entry.TrackerSig, canonical,
	); err != nil {
		return 0, classifyAppendError(err)
	}

	for _, b := range in.Balances {
		if len(b.IdentityID) != 32 {
			return 0, fmt.Errorf("storage: balance identity_id length %d, want 32", len(b.IdentityID))
		}
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO balances (identity_id, credits, last_seq, updated_at)
            VALUES (?,?,?,?)
            ON CONFLICT(identity_id) DO UPDATE SET
                credits    = excluded.credits,
                last_seq   = excluded.last_seq,
                updated_at = excluded.updated_at`,
			b.IdentityID, b.Credits, b.LastSeq, b.UpdatedAt,
		); err != nil {
			return 0, fmt.Errorf("storage: upsert balance: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("storage: commit: %w", err)
	}
	return body.Seq, nil
}

// nullableBytes converts an empty signature slice to NULL so the DB stores
// "no signature" distinguishably from "an empty 64-byte signature" (which
// the per-kind matrix never produces, but the receiver still rejects).
func nullableBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// classifyAppendError maps SQLite UNIQUE / PRIMARY KEY violations to
// ErrAlreadyExists. Other errors are wrapped verbatim. We match on the
// error message rather than introspecting the driver's typed error
// because modernc.org/sqlite returns a generic *sqlite.Error that
// surfaces the constraint name in the message.
func classifyAppendError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "UNIQUE constraint failed: entries.hash") ||
		strings.Contains(msg, "UNIQUE constraint failed: entries.seq") ||
		(strings.Contains(msg, "PRIMARY KEY") && strings.Contains(msg, "entries")) {
		return ErrAlreadyExists
	}
	return fmt.Errorf("storage: append insert: %w", err)
}

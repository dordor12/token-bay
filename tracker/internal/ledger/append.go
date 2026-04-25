package ledger

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
	"github.com/token-bay/token-bay/tracker/internal/ledger/entry"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

// ErrStaleTip means the entry's prev_hash / seq don't match the current
// chain tip. The caller should re-collect counterparty sigs against fresh
// (prev_hash, seq) and retry. Returned by AppendUsage and AppendTransferOut;
// IssueStarterGrant retries internally.
var ErrStaleTip = errors.New("ledger: entry prev_hash / seq does not match current tip")

// ErrInsufficientBalance means applying the entry's signed delta would
// drive an identity's balance below the permitted floor (v1: zero — no
// overdraft beyond the starter grant band, which is also zero in v1).
var ErrInsufficientBalance = errors.New("ledger: balance would go below permitted floor")

// appendInput is the orchestrator-internal request to appendEntry.
type appendInput struct {
	body        *tbproto.EntryBody
	consumerSig []byte // empty if no consumer signs this kind
	consumerPub ed25519.PublicKey
	seederSig   []byte // empty if no seeder signs this kind
	seederPub   ed25519.PublicKey
	deltas      []balanceDelta // 0..2 — applied to current balances to compute new credits
}

// balanceDelta is a signed change to one identity's credits.
type balanceDelta struct {
	identityID []byte
	delta      int64
}

// appendEntry is the chain-integrity-preserving core of every public
// Append* method. Holds Ledger.mu for the entire critical section: tip
// read, body validation, sig verification, tracker signing, balance
// arithmetic, and the storage transaction commit.
//
// The caller fills body.PrevHash + body.Seq before calling — this method
// verifies they match the current tip and returns ErrStaleTip if not.
// Returning the lock and asking the caller to retry is correct because
// counterparty sigs (consumer/seeder) are over the body bytes which include
// PrevHash + Seq; a fresh tip means fresh sigs are required.
//
// For tracker-only-signed kinds (STARTER_GRANT) where rebuilding is cheap
// and there are no counterparty sigs to invalidate, callers should use
// appendEntryWithBuilder instead — it constructs the body inside the lock
// and avoids the stale-tip dance entirely.
func (l *Ledger) appendEntry(ctx context.Context, in appendInput) (*tbproto.Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.appendLocked(ctx, in, true /*verifyPreFilled*/)
}

// appendEntryWithBuilder reads the current tip inside the lock, calls
// build(prev, seq) to construct the body + balance deltas, and appends.
// Used by tracker-only-signed kinds where the body has no external sigs
// over (prev, seq). Eliminates ErrStaleTip — a winning goroutine always
// gets the freshest tip.
func (l *Ledger) appendEntryWithBuilder(
	ctx context.Context,
	build func(prev []byte, seq uint64, now uint64) (appendInput, error),
) (*tbproto.Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	tipSeq, tipHash, hasTip, err := l.store.Tip(ctx)
	if err != nil {
		return nil, fmt.Errorf("ledger: read tip: %w", err)
	}
	prev := tipHash
	nextSeq := tipSeq + 1
	if !hasTip {
		prev = make([]byte, 32)
		nextSeq = 1
	}

	in, err := build(prev, nextSeq, uint64(l.nowFn().Unix()))
	if err != nil {
		return nil, err
	}
	return l.appendLocked(ctx, in, false /*verifyPreFilled*/)
}

// appendLocked is the lock-already-held core. verifyPreFilled selects
// between "caller pre-filled body.PrevHash+Seq, mismatch is ErrStaleTip"
// and "we just filled them inside the lock, mismatch is a programmer
// error".
func (l *Ledger) appendLocked(ctx context.Context, in appendInput, verifyPreFilled bool) (*tbproto.Entry, error) {
	if in.body == nil {
		return nil, errors.New("ledger: appendEntry requires non-nil body")
	}

	if verifyPreFilled {
		tipSeq, tipHash, hasTip, err := l.store.Tip(ctx)
		if err != nil {
			return nil, fmt.Errorf("ledger: read tip: %w", err)
		}
		expectedPrev := tipHash
		expectedSeq := tipSeq + 1
		if !hasTip {
			expectedPrev = make([]byte, 32)
			expectedSeq = 1
		}
		if !bytes.Equal(in.body.PrevHash, expectedPrev) || in.body.Seq != expectedSeq {
			return nil, ErrStaleTip
		}
	}

	if err := tbproto.ValidateEntryBody(in.body); err != nil {
		return nil, fmt.Errorf("ledger: validate body: %w", err)
	}

	if len(in.consumerSig) != 0 {
		if !signing.VerifyEntry(in.consumerPub, in.body, in.consumerSig) {
			return nil, errors.New("ledger: consumer_sig invalid")
		}
	}
	if len(in.seederSig) != 0 {
		if !signing.VerifyEntry(in.seederPub, in.body, in.seederSig) {
			return nil, errors.New("ledger: seeder_sig invalid")
		}
	}

	trackerSig, err := signing.SignEntry(l.trackerKey, in.body)
	if err != nil {
		return nil, fmt.Errorf("ledger: tracker sign: %w", err)
	}

	balances, err := l.applyDeltas(ctx, in.body.Seq, in.body.Timestamp, in.deltas)
	if err != nil {
		return nil, err
	}

	hash, err := entry.Hash(in.body)
	if err != nil {
		return nil, fmt.Errorf("ledger: hash: %w", err)
	}

	e := &tbproto.Entry{
		Body:        in.body,
		ConsumerSig: in.consumerSig,
		SeederSig:   in.seederSig,
		TrackerSig:  trackerSig,
	}
	if _, err := l.store.AppendEntry(ctx, storage.AppendInput{
		Entry:    e,
		Hash:     hash,
		Balances: balances,
	}); err != nil {
		return nil, fmt.Errorf("ledger: append: %w", err)
	}
	return e, nil
}

// applyDeltas reads each affected identity's current balance, applies the
// signed delta, and returns the typed BalanceUpdate slice for storage.
// Returns ErrInsufficientBalance if any new balance would go below 0.
//
// v1 hard-codes the starter-grant overdraft band to zero — no balance is
// allowed to go negative regardless of the identity's grant history. v2
// can replace this floor with a per-identity policy lookup.
func (l *Ledger) applyDeltas(ctx context.Context, seq, timestamp uint64, deltas []balanceDelta) ([]storage.BalanceUpdate, error) {
	out := make([]storage.BalanceUpdate, 0, len(deltas))
	for _, d := range deltas {
		cur, _, err := l.store.Balance(ctx, d.identityID)
		if err != nil {
			return nil, fmt.Errorf("ledger: read balance: %w", err)
		}
		next := cur.Credits + d.delta
		if next < 0 {
			return nil, fmt.Errorf("%w: identity=%x current=%d delta=%d",
				ErrInsufficientBalance, d.identityID, cur.Credits, d.delta)
		}
		out = append(out, storage.BalanceUpdate{
			IdentityID: d.identityID,
			Credits:    next,
			LastSeq:    seq,
			UpdatedAt:  timestamp,
		})
	}
	return out, nil
}

package ledger

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/ledger/entry"
)

// ErrTransferRefExists means a transfer entry with the same TransferRef
// is already on the chain. v1 callers MUST treat this as an idempotent
// success at the federation layer; the ledger never returns it today
// (the in-memory federation caches handle within-process retries).
// v2 will detect on-chain duplicates by an indexed lookup once
// tracker/internal/ledger/storage adds idx_entries_ref_kind. Reserved
// here so federation code can prepare for the v2 contract without
// a follow-up rebase.
var ErrTransferRefExists = errors.New("ledger: transfer ref already on chain")

// TransferOutRecord is the typed input to AppendTransferOut. The caller
// has already collected ConsumerSig over the EntryBody bytes derived from
// these fields plus PrevHash + Seq.
type TransferOutRecord struct {
	PrevHash    []byte // 32 bytes
	Seq         uint64
	ConsumerID  []byte // 32 bytes — the identity moving credits out
	Amount      uint64 // absolute value; debited from consumer
	Timestamp   uint64
	TransferRef []byte // 32 bytes — transfer UUID padded
	ConsumerSig []byte // 64 bytes; required (transfer_out cannot use ConsumerSigMissing)
	ConsumerPub ed25519.PublicKey
}

// AppendTransferOut moves Amount credits out of ConsumerID's balance for
// a cross-region transfer. The peer region's tracker will record the
// matching TRANSFER_IN entry against the same TransferRef.
//
// Returns ErrStaleTip if the body's (PrevHash, Seq) no longer matches
// current tip; caller re-collects ConsumerSig and retries.
func (l *Ledger) AppendTransferOut(ctx context.Context, r TransferOutRecord) (*tbproto.Entry, error) {
	if len(r.ConsumerSig) == 0 || len(r.ConsumerPub) != ed25519.PublicKeySize {
		return nil, errors.New("ledger: AppendTransferOut requires consumer sig + pubkey")
	}
	if r.Amount == 0 {
		return nil, errors.New("ledger: transfer_out amount must be > 0")
	}
	delta, err := signedAmount(r.Amount)
	if err != nil {
		return nil, err
	}

	body, err := entry.BuildTransferOutEntry(entry.TransferOutInput{
		PrevHash:    r.PrevHash,
		Seq:         r.Seq,
		ConsumerID:  r.ConsumerID,
		Amount:      r.Amount,
		Timestamp:   r.Timestamp,
		TransferRef: r.TransferRef,
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: build transfer_out: %w", err)
	}

	return l.appendEntry(ctx, appendInput{
		body:        body,
		consumerSig: r.ConsumerSig,
		consumerPub: r.ConsumerPub,
		deltas:      []balanceDelta{{identityID: r.ConsumerID, delta: -delta}},
	})
}

// TransferInRecord is the typed input to AppendTransferIn. Unlike
// AppendTransferOut there is no consumer signature: the entry is
// tracker-signed only, and its authority comes from the peer-region's
// TRANSFER_PROOF that the federation layer has already verified before
// invoking this hook.
//
// IdentityID is the recipient of the credit. The on-chain entry
// zero-fills consumer_id and seeder_id per the per-kind matrix; the
// balance delta is routed via IdentityID directly without leaving any
// counterparty trace on the entry body itself.
type TransferInRecord struct {
	PrevHash    []byte // 32 bytes
	Seq         uint64
	IdentityID  []byte // 32 bytes — recipient of the credit
	Amount      uint64 // absolute value; credited
	Timestamp   uint64
	TransferRef []byte // 32 bytes — same nonce/UUID as the source's transfer_out
}

// AppendTransferIn credits Amount to IdentityID and records a
// transfer_in entry whose Ref == TransferRef. The on-chain entry
// zero-fills consumer_id and seeder_id; the balance delta is routed via
// IdentityID directly.
//
// Returns ErrStaleTip if PrevHash/Seq don't match the current tip.
// v1 has no on-chain idempotency check at the ledger layer; the
// federation layer's in-memory pending-and-issued maps cover
// within-process retries. Persistent idempotency is a follow-up (see
// the federation cross-region transfer subsystem spec §14).
func (l *Ledger) AppendTransferIn(ctx context.Context, r TransferInRecord) (*tbproto.Entry, error) {
	if r.Amount == 0 {
		return nil, errors.New("ledger: transfer_in amount must be > 0")
	}
	delta, err := signedAmount(r.Amount)
	if err != nil {
		return nil, err
	}

	body, err := entry.BuildTransferInEntry(entry.TransferInInput{
		PrevHash:    r.PrevHash,
		Seq:         r.Seq,
		Amount:      r.Amount,
		Timestamp:   r.Timestamp,
		TransferRef: r.TransferRef,
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: build transfer_in: %w", err)
	}

	return l.appendEntry(ctx, appendInput{
		body:   body,
		deltas: []balanceDelta{{identityID: r.IdentityID, delta: delta}},
	})
}

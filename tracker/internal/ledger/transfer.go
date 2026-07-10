package ledger

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"

	fed "github.com/token-bay/token-bay/shared/federation"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
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

// ErrTransferIdentityMismatch means the transfer_out's debited identity
// is not the enrollment identity of the presented consumer pubkey —
// i.e. sha256(DER SubjectPublicKeyInfo of ConsumerPub) != ConsumerID.
// Without this binding any key holder could drain any identity's balance
// by signing an intent that names a victim identity.
var ErrTransferIdentityMismatch = errors.New("ledger: consumer pubkey does not match debited identity (sha256 of DER SPKI)")

// TransferOutRecord is the typed input to AppendTransferOut. The caller
// (federation) has already collected ConsumerSig over the
// sequencing-independent transfer-proof-request intent
// (fed.CanonicalTransferProofRequestPreSig) — NOT over the EntryBody.
// AppendTransferOut reconstructs the exact TransferProofRequest from
// these fields and re-verifies the sig against it before appending.
//
// The canonical preimage covers every TransferProofRequest field except
// consumer_sig: source_tracker_id, dest_tracker_id, identity_id
// (= ConsumerID), amount, nonce (= TransferRef), consumer_pub, and
// timestamp (= Timestamp — the record's timestamp is both the EntryBody
// timestamp and the request timestamp the consumer signed).
type TransferOutRecord struct {
	PrevHash    []byte // 32 bytes
	Seq         uint64
	ConsumerID  []byte // 32 bytes — the identity moving credits out; MUST equal sha256(DER SPKI of ConsumerPub)
	Amount      uint64 // absolute value; debited from consumer
	Timestamp   uint64 // EntryBody timestamp AND TransferProofRequest.timestamp
	TransferRef []byte // 32 bytes — transfer nonce; becomes the entry's Ref

	// SourceTrackerID / DestTrackerID are the federation tracker ids
	// (sha256 of raw federation pubkey) from the TransferProofRequest.
	// Needed only to reconstruct the canonical intent the consumer signed.
	SourceTrackerID []byte // 32 bytes
	DestTrackerID   []byte // 32 bytes

	ConsumerSig []byte // 64 bytes; required — sig over CanonicalTransferProofRequestPreSig
	ConsumerPub ed25519.PublicKey
}

// AppendTransferOut moves Amount credits out of ConsumerID's balance for
// a cross-region transfer. The peer region's tracker will record the
// matching TRANSFER_IN entry against the same TransferRef.
//
// Authorization is two checks, both before any balance arithmetic:
//
//  1. Identity binding: ConsumerID must equal SHA-256 of the DER
//     SubjectPublicKeyInfo of ConsumerPub — the same encoding
//     server.SPKIToIdentityID derives from the client cert at enrollment.
//     Otherwise ErrTransferIdentityMismatch.
//  2. Consumer intent: ConsumerSig must verify over
//     fed.CanonicalTransferProofRequestPreSig of the TransferProofRequest
//     reconstructed from the record — NOT over the EntryBody. This is the
//     sig federation actually holds; like USAGE, participant authz is over
//     a sequencing-independent canonical intent.
//
// Returns ErrStaleTip if the body's (PrevHash, Seq) no longer matches the
// current tip; because the intent sig is sequencing-independent, the
// caller refreshes (PrevHash, Seq) and retries with the SAME ConsumerSig.
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

	// (1) Bind the debited identity to the presented pubkey.
	spki, err := x509.MarshalPKIXPublicKey(r.ConsumerPub)
	if err != nil {
		return nil, fmt.Errorf("ledger: marshal consumer pubkey SPKI: %w", err)
	}
	pubID := sha256.Sum256(spki)
	if !bytes.Equal(pubID[:], r.ConsumerID) {
		return nil, fmt.Errorf("%w: pubkey identity=%x debited identity=%x",
			ErrTransferIdentityMismatch, pubID, r.ConsumerID)
	}

	// (2) Verify the consumer's authorization over the reconstructed
	// transfer-proof-request intent. The canonical preimage covers every
	// request field except consumer_sig; a single byte of drift here means
	// the federation-supplied sig cannot verify, so the reconstruction must
	// mirror fed.CanonicalTransferProofRequestPreSig exactly.
	canonical, err := fed.CanonicalTransferProofRequestPreSig(&fed.TransferProofRequest{
		SourceTrackerId: r.SourceTrackerID,
		DestTrackerId:   r.DestTrackerID,
		IdentityId:      r.ConsumerID,
		Amount:          r.Amount,
		Nonce:           r.TransferRef,
		ConsumerPub:     r.ConsumerPub,
		Timestamp:       r.Timestamp,
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: canonical transfer-proof-request: %w", err)
	}
	if !signing.Verify(r.ConsumerPub, canonical, r.ConsumerSig) {
		return nil, errors.New("ledger: consumer_sig invalid over transfer-proof-request intent")
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
		// Verified above over the transfer-proof-request intent;
		// appendLocked stores the sig verbatim without re-verifying it
		// against the EntryBody. The tracker sig over the EntryBody is
		// unaffected.
		participantSigsPreVerified: true,
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

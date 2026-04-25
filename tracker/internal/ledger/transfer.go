package ledger

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/ledger/entry"
)

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

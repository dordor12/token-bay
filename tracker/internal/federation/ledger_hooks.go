// Package federation: LedgerHooks is the cross-region transfer dependency.
//
// Production binding wires through *ledger.Ledger via a thin adapter (set
// up at startup in tracker/cmd/token-bay-tracker/run_cmd.go). Tests pass
// in fakes that record calls. A nil LedgerHooks Deps disables the
// transfer subsystem; StartTransfer returns ErrTransferDisabled and
// inbound transfer kinds are rejected.
package federation

import (
	"context"
	"crypto/ed25519"
)

// LedgerHooks is the federation-side view of the ledger orchestrator.
// Keeping this interface small lets federation depend on a slim, well-
// scoped contract instead of the full *ledger.Ledger surface.
type LedgerHooks interface {
	// AppendTransferOut debits Amount from IdentityID's balance and
	// records a transfer_out entry with TransferRef = Nonce. Returns
	// the chain-tip hash, the new entry's seq, and the timestamp at
	// which the entry was committed.
	AppendTransferOut(ctx context.Context, in TransferOutHookIn) (TransferOutHookOut, error)

	// AppendTransferIn credits Amount to IdentityID and records a
	// transfer_in entry with TransferRef = Nonce. The federation layer
	// invokes this only after verifying the source tracker's signature
	// on the proof.
	AppendTransferIn(ctx context.Context, in TransferInHookIn) error
}

// TransferOutHookIn is what the federation layer hands to AppendTransferOut.
type TransferOutHookIn struct {
	IdentityID  [32]byte
	Amount      uint64
	Timestamp   uint64
	TransferRef [32]byte
	ConsumerSig []byte
	ConsumerPub ed25519.PublicKey
}

// TransferOutHookOut is what AppendTransferOut returns to the federation
// layer to populate the outbound TransferProof.
type TransferOutHookOut struct {
	ChainTipHash [32]byte
	Seq          uint64
}

// TransferInHookIn is what the federation layer hands to AppendTransferIn.
// IdentityID is the recipient of the credit; the on-chain entry zero-fills
// consumer/seeder ids per the per-kind matrix.
type TransferInHookIn struct {
	IdentityID  [32]byte
	Amount      uint64
	Timestamp   uint64
	TransferRef [32]byte
}

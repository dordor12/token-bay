package entry

import (
	"errors"
	"fmt"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// flagConsumerSigMissing is bit 0 of EntryBody.flags (ledger spec §3.1).
const flagConsumerSigMissing uint32 = 1 << 0

// UsageInput is the typed input to BuildUsageEntry. All byte slices must
// be exact-length per §3.1; the builder enforces this via ValidateEntryBody.
type UsageInput struct {
	PrevHash           []byte // 32 bytes
	Seq                uint64
	ConsumerID         []byte // 32 bytes
	SeederID           []byte // 32 bytes
	Model              string
	InputTokens        uint32
	OutputTokens       uint32
	CostCredits        uint64
	Timestamp          uint64
	RequestID          []byte // 16 bytes
	ConsumerSigMissing bool
}

// BuildUsageEntry constructs a USAGE-kind body and returns it after running
// ValidateEntryBody. Callers can call signing.SignEntry directly without
// re-validating.
func BuildUsageEntry(in UsageInput) (*tbproto.EntryBody, error) {
	if in.Model == "" {
		return nil, errors.New("entry: BuildUsageEntry: model is empty")
	}
	body := &tbproto.EntryBody{
		PrevHash:     in.PrevHash,
		Seq:          in.Seq,
		Kind:         tbproto.EntryKind_ENTRY_KIND_USAGE,
		ConsumerId:   in.ConsumerID,
		SeederId:     in.SeederID,
		Model:        in.Model,
		InputTokens:  in.InputTokens,
		OutputTokens: in.OutputTokens,
		CostCredits:  in.CostCredits,
		Timestamp:    in.Timestamp,
		RequestId:    in.RequestID,
		Ref:          make([]byte, 32),
	}
	if in.ConsumerSigMissing {
		body.Flags |= flagConsumerSigMissing
	}
	if err := tbproto.ValidateEntryBody(body); err != nil {
		return nil, fmt.Errorf("entry: BuildUsageEntry: %w", err)
	}
	return body, nil
}

// TransferOutInput — typed input for BuildTransferOutEntry.
type TransferOutInput struct {
	PrevHash    []byte // 32
	Seq         uint64
	ConsumerID  []byte // 32 — the identity moving credits out
	Amount      uint64 // absolute value; semantic sign is "debit"
	Timestamp   uint64
	TransferRef []byte // 32 — transfer UUID padded
}

// BuildTransferOutEntry constructs a TRANSFER_OUT-kind body. Seeder/model/
// request_id slots are zero-filled per the per-kind matrix.
func BuildTransferOutEntry(in TransferOutInput) (*tbproto.EntryBody, error) {
	body := &tbproto.EntryBody{
		PrevHash:    in.PrevHash,
		Seq:         in.Seq,
		Kind:        tbproto.EntryKind_ENTRY_KIND_TRANSFER_OUT,
		ConsumerId:  in.ConsumerID,
		SeederId:    make([]byte, 32),
		CostCredits: in.Amount,
		Timestamp:   in.Timestamp,
		RequestId:   make([]byte, 16),
		Ref:         in.TransferRef,
	}
	if err := tbproto.ValidateEntryBody(body); err != nil {
		return nil, fmt.Errorf("entry: BuildTransferOutEntry: %w", err)
	}
	return body, nil
}

// TransferInInput — typed input for BuildTransferInEntry. Both counterparty
// IDs are zero on a transfer_in: the local tracker is the only signer.
type TransferInInput struct {
	PrevHash    []byte // 32
	Seq         uint64
	Amount      uint64
	Timestamp   uint64
	TransferRef []byte // 32 — peer-region transfer UUID
}

// BuildTransferInEntry constructs a TRANSFER_IN-kind body.
func BuildTransferInEntry(in TransferInInput) (*tbproto.EntryBody, error) {
	body := &tbproto.EntryBody{
		PrevHash:    in.PrevHash,
		Seq:         in.Seq,
		Kind:        tbproto.EntryKind_ENTRY_KIND_TRANSFER_IN,
		ConsumerId:  make([]byte, 32),
		SeederId:    make([]byte, 32),
		CostCredits: in.Amount,
		Timestamp:   in.Timestamp,
		RequestId:   make([]byte, 16),
		Ref:         in.TransferRef,
	}
	if err := tbproto.ValidateEntryBody(body); err != nil {
		return nil, fmt.Errorf("entry: BuildTransferInEntry: %w", err)
	}
	return body, nil
}

// StarterGrantInput — typed input for BuildStarterGrantEntry.
type StarterGrantInput struct {
	PrevHash   []byte // 32
	Seq        uint64
	ConsumerID []byte // 32 — the identity receiving the grant
	Amount     uint64
	Timestamp  uint64
}

// BuildStarterGrantEntry constructs a STARTER_GRANT-kind body. ref is
// zero-filled (no transfer UUID for grants).
func BuildStarterGrantEntry(in StarterGrantInput) (*tbproto.EntryBody, error) {
	body := &tbproto.EntryBody{
		PrevHash:    in.PrevHash,
		Seq:         in.Seq,
		Kind:        tbproto.EntryKind_ENTRY_KIND_STARTER_GRANT,
		ConsumerId:  in.ConsumerID,
		SeederId:    make([]byte, 32),
		CostCredits: in.Amount,
		Timestamp:   in.Timestamp,
		RequestId:   make([]byte, 16),
		Ref:         make([]byte, 32),
	}
	if err := tbproto.ValidateEntryBody(body); err != nil {
		return nil, fmt.Errorf("entry: BuildStarterGrantEntry: %w", err)
	}
	return body, nil
}

package ledger

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/ledger/entry"
)

// UsageRecord is the typed input to AppendUsage. The caller (broker) has
// already collected ConsumerSig + SeederSig over the EntryBody bytes
// derived from these fields plus PrevHash + Seq.
//
// PrevHash + Seq must match the current chain tip; mismatch returns
// ErrStaleTip so the broker re-collects sigs against the fresh tip.
type UsageRecord struct {
	PrevHash     []byte // 32 bytes
	Seq          uint64
	ConsumerID   []byte // 32 bytes
	SeederID     []byte // 32 bytes
	Model        string
	InputTokens  uint32
	OutputTokens uint32
	CostCredits  uint64
	Timestamp    uint64
	RequestID    []byte // 16 bytes (UUID)

	// ConsumerSigMissing flags a USAGE entry where the consumer never
	// counter-signed the settlement (settlement_timeout_s expired). Set
	// when ConsumerSig is empty for this reason; ConsumerPub is then ignored.
	ConsumerSigMissing bool
	ConsumerSig        []byte // 64 bytes; or empty if ConsumerSigMissing
	ConsumerPub        ed25519.PublicKey

	SeederSig []byte // 64 bytes; required
	SeederPub ed25519.PublicKey
}

// AppendUsage records a settled USAGE entry. The body the caller
// constructed must match the current tip exactly — orchestrator returns
// ErrStaleTip on mismatch and the caller (broker) re-collects sigs.
//
// Sig verification, balance arithmetic, and tracker signing happen here
// under Ledger.mu. The caller's CostCredits is debited from the consumer
// and credited to the seeder atomically with the entry write.
func (l *Ledger) AppendUsage(ctx context.Context, r UsageRecord) (*tbproto.Entry, error) {
	if len(r.SeederSig) == 0 || len(r.SeederPub) != ed25519.PublicKeySize {
		return nil, errors.New("ledger: AppendUsage requires seeder sig + pubkey")
	}
	if !r.ConsumerSigMissing {
		if len(r.ConsumerSig) == 0 || len(r.ConsumerPub) != ed25519.PublicKeySize {
			return nil, errors.New("ledger: AppendUsage requires consumer sig + pubkey (or set ConsumerSigMissing)")
		}
	}

	body, err := entry.BuildUsageEntry(entry.UsageInput{
		PrevHash:           r.PrevHash,
		Seq:                r.Seq,
		ConsumerID:         r.ConsumerID,
		SeederID:           r.SeederID,
		Model:              r.Model,
		InputTokens:        r.InputTokens,
		OutputTokens:       r.OutputTokens,
		CostCredits:        r.CostCredits,
		Timestamp:          r.Timestamp,
		RequestID:          r.RequestID,
		ConsumerSigMissing: r.ConsumerSigMissing,
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: build usage: %w", err)
	}

	in := appendInput{
		body:      body,
		seederSig: r.SeederSig,
		seederPub: r.SeederPub,
		deltas: []balanceDelta{
			{identityID: r.ConsumerID, delta: -int64(r.CostCredits)},
			{identityID: r.SeederID, delta: int64(r.CostCredits)},
		},
	}
	if !r.ConsumerSigMissing {
		in.consumerSig = r.ConsumerSig
		in.consumerPub = r.ConsumerPub
	}

	return l.appendEntry(ctx, in)
}

package ledger

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
	"github.com/token-bay/token-bay/tracker/internal/ledger/entry"
)

// UsageRecord is the typed input to AppendUsage. The caller (broker) has
// already collected ConsumerSig + SeederSig over the sequencing-independent
// usage-assertion (signing.UsageAssertion) derived from these fields —
// NOT over the EntryBody. AppendUsage re-verifies both against the
// assertion before appending.
//
// PrevHash + Seq must match the current chain tip; mismatch returns
// ErrStaleTip. Because the participant sigs omit sequencing, the broker
// retries with the same sigs after refreshing (prev_hash, seq).
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

// AppendUsage records a settled USAGE entry. The record's (PrevHash, Seq)
// must match the current tip exactly — orchestrator returns ErrStaleTip on
// mismatch and the caller (broker) refreshes sequencing and retries with
// the same participant sigs.
//
// Participant sigs are verified over the usage-assertion here (they
// authorize the settlement economics, not the chain position); the ledger
// then owns the EntryBody — flags bit0 = ConsumerSigMissing — and only the
// tracker sig covers it. Balance arithmetic and tracker signing happen
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

	assertion := signing.UsageAssertion{
		RequestID:    r.RequestID,
		ConsumerID:   r.ConsumerID,
		SeederID:     r.SeederID,
		Model:        r.Model,
		InputTokens:  r.InputTokens,
		OutputTokens: r.OutputTokens,
		CostCredits:  r.CostCredits,
	}
	if !signing.VerifyUsageAssertion(r.SeederPub, assertion, r.SeederSig) {
		return nil, errors.New("ledger: seeder_sig invalid over usage-assertion")
	}
	if !r.ConsumerSigMissing {
		if !signing.VerifyUsageAssertion(r.ConsumerPub, assertion, r.ConsumerSig) {
			return nil, errors.New("ledger: consumer_sig invalid over usage-assertion")
		}
	}

	cost, err := signedAmount(r.CostCredits)
	if err != nil {
		return nil, err
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
			{identityID: r.ConsumerID, delta: -cost},
			{identityID: r.SeederID, delta: cost},
		},
		// Verified above over the usage-assertion; appendLocked must store
		// them verbatim without re-verifying against the EntryBody.
		participantSigsPreVerified: true,
	}
	if !r.ConsumerSigMissing {
		in.consumerSig = r.ConsumerSig
		in.consumerPub = r.ConsumerPub
	}

	return l.appendEntry(ctx, in)
}

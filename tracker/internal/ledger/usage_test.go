package ledger

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)

// usageAssertionFor derives the sequencing-independent usage-assertion
// from a UsageRecord's fields — the preimage both participants sign.
func usageAssertionFor(r UsageRecord) signing.UsageAssertion {
	return signing.UsageAssertion{
		RequestID:    r.RequestID,
		ConsumerID:   r.ConsumerID,
		SeederID:     r.SeederID,
		Model:        r.Model,
		InputTokens:  r.InputTokens,
		OutputTokens: r.OutputTokens,
		CostCredits:  r.CostCredits,
	}
}

// signedUsageRecord builds a UsageRecord matching the current tip on l,
// with consumer + seeder signatures over the usage-assertion (the
// sequencing-independent preimage — NOT the EntryBody). Returns both the
// record and its assertion so tests can inspect either.
func signedUsageRecord(
	t *testing.T,
	l *Ledger,
	consumerID, seederID []byte,
	cPub ed25519.PublicKey, cPriv ed25519.PrivateKey,
	sPub ed25519.PublicKey, sPriv ed25519.PrivateKey,
	cost uint64,
) (UsageRecord, signing.UsageAssertion) {
	t.Helper()
	prev, seq := nextTipForTest(t, l)

	rec := UsageRecord{
		PrevHash:     prev,
		Seq:          seq,
		ConsumerID:   consumerID,
		SeederID:     seederID,
		Model:        "claude-sonnet-4-6",
		InputTokens:  100,
		OutputTokens: 50,
		CostCredits:  cost,
		Timestamp:    1714000000 + seq,
		RequestID:    bytes.Repeat([]byte{byte(seq)}, 16),
		ConsumerPub:  cPub,
		SeederPub:    sPub,
	}
	assertion := usageAssertionFor(rec)

	cSig, err := signing.SignUsageAssertion(cPriv, assertion)
	require.NoError(t, err)
	sSig, err := signing.SignUsageAssertion(sPriv, assertion)
	require.NoError(t, err)
	rec.ConsumerSig = cSig
	rec.SeederSig = sSig
	return rec, assertion
}

func TestAppendUsage_HappyPath(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	consumerID := bytes.Repeat([]byte{0x11}, 32)
	seederID := bytes.Repeat([]byte{0x22}, 32)
	cPub, cPriv := labeledKeypair("consumer")
	sPub, sPriv := labeledKeypair("seeder")

	// Pre-fund the consumer.
	_, err := l.IssueStarterGrant(ctx, consumerID, 5000)
	require.NoError(t, err)

	rec, _ := signedUsageRecord(t, l, consumerID, seederID, cPub, cPriv, sPub, sPriv, 1000)
	e, err := l.AppendUsage(ctx, rec)
	require.NoError(t, err)
	require.NotNil(t, e)

	assert.Equal(t, tbproto.EntryKind_ENTRY_KIND_USAGE, e.Body.Kind)
	assert.Equal(t, uint64(2), e.Body.Seq)
	assert.Zero(t, e.Body.Flags&1, "consumer_sig_missing flag must be clear on the happy path")
	assert.NotEmpty(t, e.TrackerSig)
	assert.Equal(t, rec.ConsumerSig, e.ConsumerSig, "assertion-domain consumer sig stored verbatim")
	assert.Equal(t, rec.SeederSig, e.SeederSig, "assertion-domain seeder sig stored verbatim")

	// Balances reflect the transfer.
	cBal, ok, err := l.store.Balance(ctx, consumerID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(4000), cBal.Credits, "consumer debited by cost")

	sBal, ok, err := l.store.Balance(ctx, seederID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(1000), sBal.Credits, "seeder credited by cost")
}

func TestAppendUsage_StaleTip(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	consumerID := bytes.Repeat([]byte{0x11}, 32)
	seederID := bytes.Repeat([]byte{0x22}, 32)
	cPub, cPriv := labeledKeypair("consumer")
	sPub, sPriv := labeledKeypair("seeder")

	_, err := l.IssueStarterGrant(ctx, consumerID, 5000)
	require.NoError(t, err)

	// Build a record against the current tip.
	rec, _ := signedUsageRecord(t, l, consumerID, seederID, cPub, cPriv, sPub, sPriv, 1000)

	// Append something else to advance the tip.
	otherID := bytes.Repeat([]byte{0x33}, 32)
	_, err = l.IssueStarterGrant(ctx, otherID, 100)
	require.NoError(t, err)

	// Original record now stale.
	_, err = l.AppendUsage(ctx, rec)
	require.ErrorIs(t, err, ErrStaleTip)
}

func TestAppendUsage_RejectsBadConsumerSig(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	consumerID := bytes.Repeat([]byte{0x11}, 32)
	seederID := bytes.Repeat([]byte{0x22}, 32)
	cPub, _ := labeledKeypair("consumer")
	sPub, sPriv := labeledKeypair("seeder")
	_, otherPriv := labeledKeypair("attacker")

	_, err := l.IssueStarterGrant(ctx, consumerID, 5000)
	require.NoError(t, err)

	prev, seq := nextTipForTest(t, l)
	rec := UsageRecord{
		PrevHash: prev, Seq: seq,
		ConsumerID: consumerID, SeederID: seederID,
		Model: "claude-sonnet-4-6", CostCredits: 1000,
		Timestamp: 1714000000, RequestID: bytes.Repeat([]byte{0x33}, 16),
		ConsumerPub: cPub, SeederPub: sPub,
	}
	assertion := usageAssertionFor(rec)

	// Sign the assertion with a key that's not the consumer's.
	badConsumerSig, err := signing.SignUsageAssertion(otherPriv, assertion)
	require.NoError(t, err)
	seederSig, err := signing.SignUsageAssertion(sPriv, assertion)
	require.NoError(t, err)
	rec.ConsumerSig = badConsumerSig
	rec.SeederSig = seederSig

	_, err = l.AppendUsage(ctx, rec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "consumer_sig invalid")
}

func TestAppendUsage_RejectsBadSeederSig(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	consumerID := bytes.Repeat([]byte{0x11}, 32)
	seederID := bytes.Repeat([]byte{0x22}, 32)
	cPub, cPriv := labeledKeypair("consumer")
	sPub, _ := labeledKeypair("seeder")
	_, otherPriv := labeledKeypair("attacker")

	_, err := l.IssueStarterGrant(ctx, consumerID, 5000)
	require.NoError(t, err)

	prev, seq := nextTipForTest(t, l)
	rec := UsageRecord{
		PrevHash: prev, Seq: seq,
		ConsumerID: consumerID, SeederID: seederID,
		Model: "claude-sonnet-4-6", CostCredits: 1000,
		Timestamp: 1714000000, RequestID: bytes.Repeat([]byte{0x33}, 16),
		ConsumerPub: cPub, SeederPub: sPub,
	}
	assertion := usageAssertionFor(rec)

	consumerSig, err := signing.SignUsageAssertion(cPriv, assertion)
	require.NoError(t, err)
	// "Seeder sig" actually produced by the attacker's key — invalid.
	badSeederSig, err := signing.SignUsageAssertion(otherPriv, assertion)
	require.NoError(t, err)
	rec.ConsumerSig = consumerSig
	rec.SeederSig = badSeederSig

	_, err = l.AppendUsage(ctx, rec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "seeder_sig invalid")
}

// TestAppendUsage_ConsumerSigMissingFlag is the dispute/timeout path: the
// consumer never counter-signed, so the entry carries only the seeder's
// assertion-domain sig plus the consumer_sig_missing flag — and balances
// still move.
func TestAppendUsage_ConsumerSigMissingFlag(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	consumerID := bytes.Repeat([]byte{0x11}, 32)
	seederID := bytes.Repeat([]byte{0x22}, 32)
	sPub, sPriv := labeledKeypair("seeder")

	_, err := l.IssueStarterGrant(ctx, consumerID, 5000)
	require.NoError(t, err)

	prev, seq := nextTipForTest(t, l)
	rec := UsageRecord{
		PrevHash: prev, Seq: seq,
		ConsumerID: consumerID, SeederID: seederID,
		Model: "claude-sonnet-4-6", CostCredits: 1000,
		Timestamp: 1714000000, RequestID: bytes.Repeat([]byte{0x33}, 16),
		ConsumerSigMissing: true,
		SeederPub:          sPub,
	}
	seederSig, err := signing.SignUsageAssertion(sPriv, usageAssertionFor(rec))
	require.NoError(t, err)
	rec.SeederSig = seederSig

	e, err := l.AppendUsage(ctx, rec)
	require.NoError(t, err)
	assert.Empty(t, e.ConsumerSig)
	assert.NotEmpty(t, e.SeederSig)
	assert.NotZero(t, e.Body.Flags&1, "consumer_sig_missing flag must be set")

	cBal, ok, err := l.store.Balance(ctx, consumerID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(4000), cBal.Credits, "consumer debited on the dispute path too")

	sBal, ok, err := l.store.Balance(ctx, seederID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(1000), sBal.Credits, "seeder credited on the dispute path too")
}

// TestAppendUsage_DuplicateRequestIDRejected — USAGE request_ids are
// single-use. After a usage entry commits, a replay of the same record
// against a fresh tip (same assertion-domain sigs — they don't bind
// sequencing) must be rejected with ErrUsageRequestExists: one entry on
// chain, balances moved exactly once. This is the ledger-level defense
// against settlement replay (duplicate usage_report → double debit).
func TestAppendUsage_DuplicateRequestIDRejected(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	consumerID := bytes.Repeat([]byte{0x11}, 32)
	seederID := bytes.Repeat([]byte{0x22}, 32)
	cPub, cPriv := labeledKeypair("consumer")
	sPub, sPriv := labeledKeypair("seeder")

	_, err := l.IssueStarterGrant(ctx, consumerID, 5000)
	require.NoError(t, err)

	rec, _ := signedUsageRecord(t, l, consumerID, seederID, cPub, cPriv, sPub, sPriv, 1000)
	_, err = l.AppendUsage(ctx, rec)
	require.NoError(t, err)

	// Replay: same request_id + sigs, sequencing refreshed to the new tip.
	rec.PrevHash, rec.Seq = nextTipForTest(t, l)
	_, err = l.AppendUsage(ctx, rec)
	require.ErrorIs(t, err, ErrUsageRequestExists)

	// Exactly one usage entry on chain: tip is grant (1) + usage (2).
	tipSeq, _, hasTip, err := l.Tip(ctx)
	require.NoError(t, err)
	require.True(t, hasTip)
	assert.Equal(t, uint64(2), tipSeq, "replay must not append a second entry")

	// Balances moved exactly once.
	cBal, ok, err := l.store.Balance(ctx, consumerID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(4000), cBal.Credits, "consumer debited once, not twice")

	sBal, ok, err := l.store.Balance(ctx, seederID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(1000), sBal.Credits, "seeder credited once, not twice")
}

// TestAppendUsage_StaleTipRetrySameSigs proves the point of the
// sequencing-independent assertion: when the tip moves, the broker only
// refreshes (prev_hash, seq) and retries with the SAME participant sigs —
// no re-collection round-trip. The retry is ONE logical append: the first
// attempt failed with ErrStaleTip and committed nothing, so the request_id
// is still unused. Once the retry commits, the request_id is spent — any
// further same-sig re-append is a replay, rejected with
// ErrUsageRequestExists, never a double debit.
func TestAppendUsage_StaleTipRetrySameSigs(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	consumerID := bytes.Repeat([]byte{0x11}, 32)
	seederID := bytes.Repeat([]byte{0x22}, 32)
	cPub, cPriv := labeledKeypair("consumer")
	sPub, sPriv := labeledKeypair("seeder")

	_, err := l.IssueStarterGrant(ctx, consumerID, 5000)
	require.NoError(t, err)

	rec, _ := signedUsageRecord(t, l, consumerID, seederID, cPub, cPriv, sPub, sPriv, 1000)

	// Advance the tip so rec is stale.
	otherID := bytes.Repeat([]byte{0x33}, 32)
	_, err = l.IssueStarterGrant(ctx, otherID, 100)
	require.NoError(t, err)

	_, err = l.AppendUsage(ctx, rec)
	require.ErrorIs(t, err, ErrStaleTip)

	// Refresh sequencing only; signatures are untouched.
	rec.PrevHash, rec.Seq = nextTipForTest(t, l)
	e, err := l.AppendUsage(ctx, rec)
	require.NoError(t, err)
	assert.Equal(t, rec.ConsumerSig, e.ConsumerSig)
	assert.Equal(t, rec.SeederSig, e.SeederSig)

	// The retry committed — the request_id is now spent. Re-appending the
	// same record against a fresh tip is a replay, not a retry.
	rec.PrevHash, rec.Seq = nextTipForTest(t, l)
	_, err = l.AppendUsage(ctx, rec)
	require.ErrorIs(t, err, ErrUsageRequestExists)
}

func TestAppendUsage_InsufficientBalance(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	consumerID := bytes.Repeat([]byte{0x11}, 32)
	seederID := bytes.Repeat([]byte{0x22}, 32)
	cPub, cPriv := labeledKeypair("consumer")
	sPub, sPriv := labeledKeypair("seeder")

	// Skip starter grant — consumer has no credits.
	rec, _ := signedUsageRecord(t, l, consumerID, seederID, cPub, cPriv, sPub, sPriv, 1000)
	_, err := l.AppendUsage(ctx, rec)
	require.ErrorIs(t, err, ErrInsufficientBalance)
}

func TestAppendUsage_RejectsMissingSeederSig(t *testing.T) {
	l := openTempLedger(t)
	cPub, _ := labeledKeypair("consumer")

	rec := UsageRecord{
		PrevHash:    make([]byte, 32),
		Seq:         1,
		ConsumerID:  bytes.Repeat([]byte{0x11}, 32),
		SeederID:    bytes.Repeat([]byte{0x22}, 32),
		Model:       "claude-sonnet-4-6",
		CostCredits: 1000,
		ConsumerSig: bytes.Repeat([]byte{0x33}, 64),
		ConsumerPub: cPub,
		// SeederSig + SeederPub deliberately omitted.
	}
	_, err := l.AppendUsage(context.Background(), rec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "seeder")
}

func TestAppendUsage_RejectsMissingConsumerSigWithoutFlag(t *testing.T) {
	l := openTempLedger(t)
	sPub, _ := labeledKeypair("seeder")

	rec := UsageRecord{
		PrevHash: make([]byte, 32), Seq: 1,
		ConsumerID: bytes.Repeat([]byte{0x11}, 32),
		SeederID:   bytes.Repeat([]byte{0x22}, 32),
		Model:      "claude-sonnet-4-6", CostCredits: 1000,
		// ConsumerSig omitted, ConsumerSigMissing not set.
		SeederSig: bytes.Repeat([]byte{0x33}, 64), SeederPub: sPub,
	}
	_, err := l.AppendUsage(context.Background(), rec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "consumer")
}

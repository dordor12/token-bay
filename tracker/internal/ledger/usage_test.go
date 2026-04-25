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
	"github.com/token-bay/token-bay/tracker/internal/ledger/entry"
)

// signedUsageRecord builds a UsageRecord matching the current tip on l,
// signing the body with the consumer + seeder keypairs. Returns both the
// record and the body so tests can inspect either.
func signedUsageRecord(
	t *testing.T,
	l *Ledger,
	consumerID, seederID []byte,
	cPub ed25519.PublicKey, cPriv ed25519.PrivateKey,
	sPub ed25519.PublicKey, sPriv ed25519.PrivateKey,
	cost uint64,
) (UsageRecord, *tbproto.EntryBody) {
	t.Helper()
	prev, seq := nextTipForTest(t, l)

	body, err := entry.BuildUsageEntry(entry.UsageInput{
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
	})
	require.NoError(t, err)

	cSig, err := signing.SignEntry(cPriv, body)
	require.NoError(t, err)
	sSig, err := signing.SignEntry(sPriv, body)
	require.NoError(t, err)

	return UsageRecord{
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
		ConsumerSig:  cSig,
		ConsumerPub:  cPub,
		SeederSig:    sSig,
		SeederPub:    sPub,
	}, body
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
	assert.NotEmpty(t, e.TrackerSig)
	assert.NotEmpty(t, e.ConsumerSig)
	assert.NotEmpty(t, e.SeederSig)

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
	body, err := entry.BuildUsageEntry(entry.UsageInput{
		PrevHash:    prev,
		Seq:         seq,
		ConsumerID:  consumerID,
		SeederID:    seederID,
		Model:       "claude-sonnet-4-6",
		CostCredits: 1000,
		Timestamp:   1714000000,
		RequestID:   bytes.Repeat([]byte{0x33}, 16),
	})
	require.NoError(t, err)

	// Sign with a key that's not the consumer's.
	badConsumerSig, err := signing.SignEntry(otherPriv, body)
	require.NoError(t, err)
	seederSig, err := signing.SignEntry(sPriv, body)
	require.NoError(t, err)

	rec := UsageRecord{
		PrevHash: prev, Seq: seq,
		ConsumerID: consumerID, SeederID: seederID,
		Model: "claude-sonnet-4-6", CostCredits: 1000,
		Timestamp: 1714000000, RequestID: bytes.Repeat([]byte{0x33}, 16),
		ConsumerSig: badConsumerSig, ConsumerPub: cPub,
		SeederSig: seederSig, SeederPub: sPub,
	}
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
	body, err := entry.BuildUsageEntry(entry.UsageInput{
		PrevHash: prev, Seq: seq,
		ConsumerID: consumerID, SeederID: seederID,
		Model: "claude-sonnet-4-6", CostCredits: 1000,
		Timestamp: 1714000000, RequestID: bytes.Repeat([]byte{0x33}, 16),
	})
	require.NoError(t, err)

	consumerSig, err := signing.SignEntry(cPriv, body)
	require.NoError(t, err)
	badSeederSig, err := signing.SignEntry(otherPriv, body)
	require.NoError(t, err)

	rec := UsageRecord{
		PrevHash: prev, Seq: seq,
		ConsumerID: consumerID, SeederID: seederID,
		Model: "claude-sonnet-4-6", CostCredits: 1000,
		Timestamp: 1714000000, RequestID: bytes.Repeat([]byte{0x33}, 16),
		ConsumerSig: consumerSig, ConsumerPub: cPub,
		SeederSig: badSeederSig, SeederPub: sPub,
	}
	_, err = l.AppendUsage(ctx, rec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "seeder_sig invalid")
}

func TestAppendUsage_ConsumerSigMissingFlag(t *testing.T) {
	l := openTempLedger(t)
	ctx := context.Background()
	consumerID := bytes.Repeat([]byte{0x11}, 32)
	seederID := bytes.Repeat([]byte{0x22}, 32)
	sPub, sPriv := labeledKeypair("seeder")

	_, err := l.IssueStarterGrant(ctx, consumerID, 5000)
	require.NoError(t, err)

	prev, seq := nextTipForTest(t, l)
	body, err := entry.BuildUsageEntry(entry.UsageInput{
		PrevHash: prev, Seq: seq,
		ConsumerID: consumerID, SeederID: seederID,
		Model: "claude-sonnet-4-6", CostCredits: 1000,
		Timestamp: 1714000000, RequestID: bytes.Repeat([]byte{0x33}, 16),
		ConsumerSigMissing: true,
	})
	require.NoError(t, err)
	seederSig, err := signing.SignEntry(sPriv, body)
	require.NoError(t, err)

	rec := UsageRecord{
		PrevHash: prev, Seq: seq,
		ConsumerID: consumerID, SeederID: seederID,
		Model: "claude-sonnet-4-6", CostCredits: 1000,
		Timestamp: 1714000000, RequestID: bytes.Repeat([]byte{0x33}, 16),
		ConsumerSigMissing: true,
		SeederSig: seederSig, SeederPub: sPub,
	}
	e, err := l.AppendUsage(ctx, rec)
	require.NoError(t, err)
	assert.Empty(t, e.ConsumerSig)
	assert.NotEmpty(t, e.SeederSig)
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

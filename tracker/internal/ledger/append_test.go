package ledger

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/signing"
	"github.com/token-bay/token-bay/tracker/internal/ledger/entry"
)

// buildStarterGrantBody is a test helper that constructs a STARTER_GRANT
// EntryBody at (seq, prevHash) with a fixed identity + amount. Used to
// exercise appendEntry's chain-integrity logic in isolation.
func buildStarterGrantBody(t *testing.T, seq uint64, prevHash, consumerID []byte) *appendInput {
	t.Helper()
	body, err := entry.BuildStarterGrantEntry(entry.StarterGrantInput{
		PrevHash:   prevHash,
		Seq:        seq,
		ConsumerID: consumerID,
		Amount:     1000,
		Timestamp:  1714000000,
	})
	require.NoError(t, err)
	return &appendInput{
		body:   body,
		deltas: []balanceDelta{{identityID: consumerID, delta: 1000}},
	}
}

func TestAppendEntry_StaleTip_WrongPrevHash(t *testing.T) {
	l := openTempLedger(t)
	consumerID := bytes.Repeat([]byte{0x11}, 32)

	in := buildStarterGrantBody(t, 1, bytes.Repeat([]byte{0xFF}, 32), consumerID)
	_, err := l.appendEntry(context.Background(), *in)
	require.ErrorIs(t, err, ErrStaleTip)
}

func TestAppendEntry_StaleTip_WrongSeq(t *testing.T) {
	l := openTempLedger(t)
	consumerID := bytes.Repeat([]byte{0x11}, 32)

	in := buildStarterGrantBody(t, 99, make([]byte, 32), consumerID)
	_, err := l.appendEntry(context.Background(), *in)
	require.ErrorIs(t, err, ErrStaleTip)
}

func TestAppendEntry_GenesisHappyPath(t *testing.T) {
	l := openTempLedger(t)
	consumerID := bytes.Repeat([]byte{0x11}, 32)

	in := buildStarterGrantBody(t, 1, make([]byte, 32), consumerID)
	got, err := l.appendEntry(context.Background(), *in)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.NotEmpty(t, got.TrackerSig)
	assert.Empty(t, got.ConsumerSig)
	assert.Empty(t, got.SeederSig)
}

func TestAppendEntry_RejectsBadConsumerSig(t *testing.T) {
	l := openTempLedger(t)
	consumerID := bytes.Repeat([]byte{0x11}, 32)
	seederID := bytes.Repeat([]byte{0x22}, 32)
	cPub, _ := labeledKeypair("consumer")
	sPub, sPriv := labeledKeypair("seeder")

	body, err := entry.BuildUsageEntry(entry.UsageInput{
		PrevHash:    make([]byte, 32),
		Seq:         1,
		ConsumerID:  consumerID,
		SeederID:    seederID,
		Model:       "claude-sonnet-4-6",
		CostCredits: 0, // zero so insufficient-balance doesn't fire first
		Timestamp:   1714000000,
		RequestID:   bytes.Repeat([]byte{0x33}, 16),
	})
	require.NoError(t, err)

	// Sign the body with seeder's key but pass it as consumer's sig — invalid.
	badConsumerSig, err := signing.SignEntry(sPriv, body)
	require.NoError(t, err)
	seederSig, err := signing.SignEntry(sPriv, body)
	require.NoError(t, err)

	_, err = l.appendEntry(context.Background(), appendInput{
		body:        body,
		consumerSig: badConsumerSig,
		consumerPub: cPub,
		seederSig:   seederSig,
		seederPub:   sPub,
		deltas: []balanceDelta{
			{identityID: consumerID, delta: 0},
			{identityID: seederID, delta: 0},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "consumer_sig invalid")
}

func TestAppendEntry_RejectsBadSeederSig(t *testing.T) {
	l := openTempLedger(t)
	consumerID := bytes.Repeat([]byte{0x11}, 32)
	seederID := bytes.Repeat([]byte{0x22}, 32)
	cPub, cPriv := labeledKeypair("consumer")
	sPub, _ := labeledKeypair("seeder")

	body, err := entry.BuildUsageEntry(entry.UsageInput{
		PrevHash:    make([]byte, 32),
		Seq:         1,
		ConsumerID:  consumerID,
		SeederID:    seederID,
		Model:       "claude-sonnet-4-6",
		CostCredits: 0,
		Timestamp:   1714000000,
		RequestID:   bytes.Repeat([]byte{0x33}, 16),
	})
	require.NoError(t, err)

	consumerSig, err := signing.SignEntry(cPriv, body)
	require.NoError(t, err)
	// "Seeder sig" actually signed by consumer key — invalid.
	badSeederSig, err := signing.SignEntry(cPriv, body)
	require.NoError(t, err)

	_, err = l.appendEntry(context.Background(), appendInput{
		body:        body,
		consumerSig: consumerSig,
		consumerPub: cPub,
		seederSig:   badSeederSig,
		seederPub:   sPub,
		deltas: []balanceDelta{
			{identityID: consumerID, delta: 0},
			{identityID: seederID, delta: 0},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "seeder_sig invalid")
}

func TestAppendEntry_InsufficientBalance(t *testing.T) {
	l := openTempLedger(t)
	consumerID := bytes.Repeat([]byte{0x11}, 32)

	body, err := entry.BuildStarterGrantEntry(entry.StarterGrantInput{
		PrevHash:   make([]byte, 32),
		Seq:        1,
		ConsumerID: consumerID,
		Amount:     1000,
		Timestamp:  1714000000,
	})
	require.NoError(t, err)

	// Synthesize a debit on an identity with zero balance.
	_, err = l.appendEntry(context.Background(), appendInput{
		body:   body,
		deltas: []balanceDelta{{identityID: consumerID, delta: -1}},
	})
	require.ErrorIs(t, err, ErrInsufficientBalance)
}

func TestAppendEntry_NilBody(t *testing.T) {
	l := openTempLedger(t)
	_, err := l.appendEntry(context.Background(), appendInput{})
	require.Error(t, err)
}

// confirmCompiles is a no-op that prevents the "ed25519 unused import"
// warning some IDEs flash if all signing tests are removed.
var _ = ed25519.PublicKeySize

package entry

import (
	"crypto/ed25519"
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)

// keypair derives a deterministic Ed25519 keypair from a label, so each
// test gets a stable but distinct signer.
func keypair(label string) (ed25519.PublicKey, ed25519.PrivateKey) {
	seed := sha256.Sum256([]byte("entry-test-seed-" + label))
	priv := ed25519.NewKeyFromSeed(seed[:])
	return priv.Public().(ed25519.PublicKey), priv
}

// signedUsage builds a fully-signed USAGE entry from the standard fixture.
func signedUsage(t *testing.T) (*tbproto.Entry, Pubkeys) {
	t.Helper()
	body, err := BuildUsageEntry(UsageInput{
		PrevHash:   b32(0x01), Seq: 42,
		ConsumerID: b32(0x11), SeederID: b32(0x22),
		Model: "claude-sonnet-4-6", Timestamp: 1714000100,
		RequestID: b16(0x33),
	})
	require.NoError(t, err)

	cPub, cPriv := keypair("consumer")
	sPub, sPriv := keypair("seeder")
	tPub, tPriv := keypair("tracker")
	cSig, _ := signing.SignEntry(cPriv, body)
	sSig, _ := signing.SignEntry(sPriv, body)
	tSig, _ := signing.SignEntry(tPriv, body)

	return &tbproto.Entry{
			Body:        body,
			ConsumerSig: cSig,
			SeederSig:   sSig,
			TrackerSig:  tSig,
		}, Pubkeys{
			Consumer: cPub,
			Seeder:   sPub,
			Tracker:  tPub,
		}
}

func TestVerifyAll_UsageHappyPath(t *testing.T) {
	e, keys := signedUsage(t)
	require.NoError(t, VerifyAll(e, keys))
}

func TestVerifyAll_NilEntry(t *testing.T) {
	_, keys := signedUsage(t)
	require.Error(t, VerifyAll(nil, keys))
	require.Error(t, VerifyAll(&tbproto.Entry{Body: nil}, keys))
}

func TestVerifyAll_MissingTrackerSig(t *testing.T) {
	e, keys := signedUsage(t)
	e.TrackerSig = nil
	err := VerifyAll(e, keys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tracker_sig")
}

func TestVerifyAll_MissingTrackerKey(t *testing.T) {
	e, keys := signedUsage(t)
	keys.Tracker = nil
	err := VerifyAll(e, keys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tracker pubkey")
}

func TestVerifyAll_TamperedTrackerSig(t *testing.T) {
	e, keys := signedUsage(t)
	e.TrackerSig[0] ^= 0xFF
	err := VerifyAll(e, keys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tracker_sig invalid")
}

func TestVerifyAll_TamperedBody(t *testing.T) {
	e, keys := signedUsage(t)
	e.Body.CostCredits++
	require.Error(t, VerifyAll(e, keys))
}

func TestVerifyAll_UsageMissingSeederSig(t *testing.T) {
	e, keys := signedUsage(t)
	e.SeederSig = nil
	err := VerifyAll(e, keys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "seeder_sig")
}

func TestVerifyAll_UsageBadSeederSig(t *testing.T) {
	e, keys := signedUsage(t)
	e.SeederSig[0] ^= 0xFF
	err := VerifyAll(e, keys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "seeder_sig invalid")
}

func TestVerifyAll_UsageMissingConsumerSig(t *testing.T) {
	e, keys := signedUsage(t)
	e.ConsumerSig = nil
	err := VerifyAll(e, keys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "consumer_sig required")
}

func TestVerifyAll_UsageBadConsumerSig(t *testing.T) {
	e, keys := signedUsage(t)
	e.ConsumerSig[0] ^= 0xFF
	err := VerifyAll(e, keys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "consumer_sig invalid")
}

func TestVerifyAll_UsageMissingConsumerKey(t *testing.T) {
	e, keys := signedUsage(t)
	keys.Consumer = nil
	err := VerifyAll(e, keys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "consumer pubkey")
}

func TestVerifyAll_UsageMissingSeederKey(t *testing.T) {
	e, keys := signedUsage(t)
	keys.Seeder = nil
	err := VerifyAll(e, keys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "seeder pubkey")
}

// USAGE with consumer_sig_missing flag — consumer_sig must be empty;
// tracker_sig + seeder_sig still required.
func TestVerifyAll_UsageWithFlagAndEmptyConsumerSig(t *testing.T) {
	body, err := BuildUsageEntry(UsageInput{
		PrevHash:   b32(0x01), Seq: 42,
		ConsumerID: b32(0x11), SeederID: b32(0x22),
		Model: "claude-sonnet-4-6", Timestamp: 1714000100,
		RequestID: b16(0x33), ConsumerSigMissing: true,
	})
	require.NoError(t, err)

	sPub, sPriv := keypair("seeder")
	tPub, tPriv := keypair("tracker")
	sSig, _ := signing.SignEntry(sPriv, body)
	tSig, _ := signing.SignEntry(tPriv, body)

	e := &tbproto.Entry{Body: body, SeederSig: sSig, TrackerSig: tSig}
	require.NoError(t, VerifyAll(e, Pubkeys{Seeder: sPub, Tracker: tPub}))
}

func TestVerifyAll_UsageWithFlagButConsumerSigPresent(t *testing.T) {
	e, keys := signedUsage(t)
	e.Body.Flags |= flagConsumerSigMissing
	// Re-sign the seeder + tracker over the modified body so they verify.
	_, sPriv := keypair("seeder")
	_, tPriv := keypair("tracker")
	e.SeederSig, _ = signing.SignEntry(sPriv, e.Body)
	e.TrackerSig, _ = signing.SignEntry(tPriv, e.Body)

	err := VerifyAll(e, keys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "consumer_sig must be empty")
}

// TRANSFER_OUT
func signedTransferOut(t *testing.T) (*tbproto.Entry, Pubkeys) {
	t.Helper()
	body, err := BuildTransferOutEntry(TransferOutInput{
		PrevHash:    b32(0x01),
		Seq:         43,
		ConsumerID:  b32(0x11),
		Amount:      1000,
		Timestamp:   1714000200,
		TransferRef: b32(0x77),
	})
	require.NoError(t, err)

	cPub, cPriv := keypair("consumer")
	tPub, tPriv := keypair("tracker")
	cSig, _ := signing.SignEntry(cPriv, body)
	tSig, _ := signing.SignEntry(tPriv, body)

	return &tbproto.Entry{Body: body, ConsumerSig: cSig, TrackerSig: tSig},
		Pubkeys{Consumer: cPub, Tracker: tPub}
}

func TestVerifyAll_TransferOutHappyPath(t *testing.T) {
	e, keys := signedTransferOut(t)
	require.NoError(t, VerifyAll(e, keys))
}

func TestVerifyAll_TransferOutStraySeederSig(t *testing.T) {
	e, keys := signedTransferOut(t)
	_, sPriv := keypair("seeder")
	e.SeederSig, _ = signing.SignEntry(sPriv, e.Body)
	err := VerifyAll(e, keys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "seeder_sig must be empty")
}

func TestVerifyAll_TransferOutMissingConsumerSig(t *testing.T) {
	e, keys := signedTransferOut(t)
	e.ConsumerSig = nil
	err := VerifyAll(e, keys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "consumer_sig required")
}

func TestVerifyAll_TransferOutBadConsumerSig(t *testing.T) {
	e, keys := signedTransferOut(t)
	e.ConsumerSig[0] ^= 0xFF
	err := VerifyAll(e, keys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "consumer_sig invalid")
}

func TestVerifyAll_TransferOutMissingConsumerKey(t *testing.T) {
	e, keys := signedTransferOut(t)
	keys.Consumer = nil
	err := VerifyAll(e, keys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "consumer pubkey")
}

func TestVerifyAll_TransferOutCannotSetConsumerSigMissingFlag(t *testing.T) {
	e, keys := signedTransferOut(t)
	e.Body.Flags |= flagConsumerSigMissing
	_, tPriv := keypair("tracker")
	e.TrackerSig, _ = signing.SignEntry(tPriv, e.Body)

	err := VerifyAll(e, keys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "transfer_out cannot set consumer_sig_missing")
}

// TRANSFER_IN: only tracker signs.
func signedTransferIn(t *testing.T) (*tbproto.Entry, Pubkeys) {
	t.Helper()
	body, err := BuildTransferInEntry(TransferInInput{
		PrevHash:    b32(0x01),
		Seq:         44,
		Amount:      1000,
		Timestamp:   1714000300,
		TransferRef: b32(0x88),
	})
	require.NoError(t, err)

	tPub, tPriv := keypair("tracker")
	tSig, _ := signing.SignEntry(tPriv, body)
	return &tbproto.Entry{Body: body, TrackerSig: tSig}, Pubkeys{Tracker: tPub}
}

func TestVerifyAll_TransferInHappyPath(t *testing.T) {
	e, keys := signedTransferIn(t)
	require.NoError(t, VerifyAll(e, keys))
}

func TestVerifyAll_TransferInStrayConsumerSig(t *testing.T) {
	e, keys := signedTransferIn(t)
	_, cPriv := keypair("consumer")
	e.ConsumerSig, _ = signing.SignEntry(cPriv, e.Body)
	err := VerifyAll(e, keys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "consumer_sig must be empty")
}

func TestVerifyAll_TransferInStraySeederSig(t *testing.T) {
	e, keys := signedTransferIn(t)
	_, sPriv := keypair("seeder")
	e.SeederSig, _ = signing.SignEntry(sPriv, e.Body)
	err := VerifyAll(e, keys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "seeder_sig must be empty")
}

// STARTER_GRANT: only tracker signs.
func TestVerifyAll_StarterGrantHappyPath(t *testing.T) {
	body, err := BuildStarterGrantEntry(StarterGrantInput{
		PrevHash:   b32(0x01),
		Seq:        1,
		ConsumerID: b32(0x11),
		Amount:     100000,
		Timestamp:  1714000000,
	})
	require.NoError(t, err)
	tPub, tPriv := keypair("tracker")
	tSig, _ := signing.SignEntry(tPriv, body)
	require.NoError(t, VerifyAll(
		&tbproto.Entry{Body: body, TrackerSig: tSig},
		Pubkeys{Tracker: tPub},
	))
}

func TestVerifyAll_StarterGrantStrayConsumerSig(t *testing.T) {
	body, err := BuildStarterGrantEntry(StarterGrantInput{
		PrevHash: b32(0x01), Seq: 1, ConsumerID: b32(0x11),
		Amount: 100000, Timestamp: 1714000000,
	})
	require.NoError(t, err)
	tPub, tPriv := keypair("tracker")
	_, cPriv := keypair("consumer")
	tSig, _ := signing.SignEntry(tPriv, body)
	cSig, _ := signing.SignEntry(cPriv, body)

	err = VerifyAll(
		&tbproto.Entry{Body: body, ConsumerSig: cSig, TrackerSig: tSig},
		Pubkeys{Tracker: tPub},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "consumer_sig must be empty")
}

// Unknown kind (synthesized, not buildable through the typed builders).
func TestVerifyAll_UnknownKind(t *testing.T) {
	body, err := BuildStarterGrantEntry(StarterGrantInput{
		PrevHash: b32(0x01), Seq: 1, ConsumerID: b32(0x11),
		Amount: 100000, Timestamp: 1714000000,
	})
	require.NoError(t, err)
	tPub, tPriv := keypair("tracker")
	body.Kind = tbproto.EntryKind(99)
	tSig, _ := signing.SignEntry(tPriv, body)

	err = VerifyAll(
		&tbproto.Entry{Body: body, TrackerSig: tSig},
		Pubkeys{Tracker: tPub},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown kind")
}

package signing

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/exhaustionproof"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// fixtureKeypair derives a deterministic Ed25519 keypair from a fixed seed.
// Used across all signing tests so signatures are reproducible.
func fixtureKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	seed := []byte("token-bay-fixture-seed-v1-000000") // 32 bytes
	require.Len(t, seed, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	return priv.Public().(ed25519.PublicKey), priv
}

func fixtureBody() *tbproto.EnvelopeBody {
	return &tbproto.EnvelopeBody{
		ProtocolVersion: uint32(tbproto.ProtocolVersion),
		ConsumerId:      bytes32(0x11),
		Model:           "claude-sonnet-4-6",
		MaxInputTokens:  4096,
		MaxOutputTokens: 1024,
		Tier:            tbproto.PrivacyTier_PRIVACY_TIER_STANDARD,
		BodyHash:        bytes32(0x22),
		ExhaustionProof: &exhaustionproof.ExhaustionProofV1{
			StopFailure: &exhaustionproof.StopFailure{Matcher: "rate_limit", At: 1714000000, ErrorShape: []byte(`{}`)},
			UsageProbe:  &exhaustionproof.UsageProbe{At: 1714000010, Output: []byte(`x`)},
			CapturedAt:  1714000020,
			Nonce:       []byte("proof-nonce-1234"),
		},
		BalanceProof: &tbproto.SignedBalanceSnapshot{
			Body: &tbproto.BalanceSnapshotBody{
				IdentityId: bytes32(0x33), Credits: 99, ChainTipHash: bytes32(0x44),
				ChainTipSeq: 1, IssuedAt: 1714000000, ExpiresAt: 1714000600,
			},
			TrackerSig: bytes64(0x55),
		},
		CapturedAt: 1714000025,
		Nonce:      []byte("envelope-nonce12"),
	}
}

func bytes32(b byte) []byte { return repeat(b, 32) }
func bytes64(b byte) []byte { return repeat(b, 64) }
func repeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func TestDeterministicMarshal_Stable(t *testing.T) {
	body := fixtureBody()
	a, err := DeterministicMarshal(body)
	require.NoError(t, err)
	b, err := DeterministicMarshal(body)
	require.NoError(t, err)
	assert.Equal(t, a, b, "DeterministicMarshal must be byte-stable across calls")
}

func TestDeterministicMarshal_NilReturnsError(t *testing.T) {
	_, err := DeterministicMarshal(nil)
	require.Error(t, err)
}

func TestSignVerifyEnvelope_RoundTrip(t *testing.T) {
	pub, priv := fixtureKeypair(t)
	body := fixtureBody()

	sig, err := SignEnvelope(priv, body)
	require.NoError(t, err)
	require.Len(t, sig, ed25519.SignatureSize)

	signed := &tbproto.EnvelopeSigned{Body: body, ConsumerSig: sig}
	assert.True(t, VerifyEnvelope(pub, signed))
}

func TestVerifyEnvelope_TamperedBodyFails(t *testing.T) {
	pub, priv := fixtureKeypair(t)
	body := fixtureBody()
	sig, err := SignEnvelope(priv, body)
	require.NoError(t, err)

	body.Model = "claude-sonnet-4-6-tampered"
	signed := &tbproto.EnvelopeSigned{Body: body, ConsumerSig: sig}
	assert.False(t, VerifyEnvelope(pub, signed))
}

func TestVerifyEnvelope_TamperedSigFails(t *testing.T) {
	pub, priv := fixtureKeypair(t)
	body := fixtureBody()
	sig, err := SignEnvelope(priv, body)
	require.NoError(t, err)
	sig[0] ^= 0xFF

	signed := &tbproto.EnvelopeSigned{Body: body, ConsumerSig: sig}
	assert.False(t, VerifyEnvelope(pub, signed))
}

func TestVerifyEnvelope_WrongKeyFails(t *testing.T) {
	_, priv := fixtureKeypair(t)
	body := fixtureBody()
	sig, err := SignEnvelope(priv, body)
	require.NoError(t, err)

	otherSeed := make([]byte, ed25519.SeedSize)
	otherSeed[0] = 0xFF
	otherPriv := ed25519.NewKeyFromSeed(otherSeed)
	otherPub := otherPriv.Public().(ed25519.PublicKey)

	signed := &tbproto.EnvelopeSigned{Body: body, ConsumerSig: sig}
	assert.False(t, VerifyEnvelope(otherPub, signed))
}

func TestVerifyEnvelope_NilSafety(t *testing.T) {
	pub, _ := fixtureKeypair(t)
	assert.False(t, VerifyEnvelope(pub, nil))
	assert.False(t, VerifyEnvelope(pub, &tbproto.EnvelopeSigned{Body: nil, ConsumerSig: bytes64(0x00)}))
	assert.False(t, VerifyEnvelope(nil, &tbproto.EnvelopeSigned{Body: fixtureBody(), ConsumerSig: bytes64(0x00)}))
}

func TestSignEnvelope_BadKeyLength(t *testing.T) {
	_, err := SignEnvelope(ed25519.PrivateKey{1, 2, 3}, fixtureBody())
	require.Error(t, err)
}

func TestSignEnvelope_NilBody(t *testing.T) {
	_, priv := fixtureKeypair(t)
	_, err := SignEnvelope(priv, nil)
	require.Error(t, err)
}

func TestSignVerifyBalanceSnapshot_RoundTrip(t *testing.T) {
	pub, priv := fixtureKeypair(t)
	body := &tbproto.BalanceSnapshotBody{
		IdentityId: bytes32(0xAA), Credits: 500, ChainTipHash: bytes32(0xBB),
		ChainTipSeq: 7, IssuedAt: 1714000000, ExpiresAt: 1714000600,
	}

	sig, err := SignBalanceSnapshot(priv, body)
	require.NoError(t, err)
	signed := &tbproto.SignedBalanceSnapshot{Body: body, TrackerSig: sig}
	assert.True(t, VerifyBalanceSnapshot(pub, signed))

	// Tamper detection.
	body.Credits = 999
	assert.False(t, VerifyBalanceSnapshot(pub, signed))
}

func TestVerifyBalanceSnapshot_NilSafety(t *testing.T) {
	pub, _ := fixtureKeypair(t)
	assert.False(t, VerifyBalanceSnapshot(pub, nil))
	assert.False(t, VerifyBalanceSnapshot(pub, &tbproto.SignedBalanceSnapshot{Body: nil}))
}

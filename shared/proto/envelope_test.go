package proto

import (
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/exhaustionproof"
)

// fixtureEnvelopeBody returns a fully-populated EnvelopeBody with
// deterministic field values — used by round-trip, sign/verify, and the
// golden-fixture tests across the shared/ packages.
func fixtureEnvelopeBody() *EnvelopeBody {
	return &EnvelopeBody{
		ProtocolVersion: uint32(ProtocolVersion),
		ConsumerId:      make32(0x11),
		Model:           "claude-sonnet-4-6",
		MaxInputTokens:  4096,
		MaxOutputTokens: 1024,
		Tier:            PrivacyTier_PRIVACY_TIER_STANDARD,
		BodyHash:        make32(0x22),
		ExhaustionProof: &exhaustionproof.ExhaustionProofV1{
			StopFailure: &exhaustionproof.StopFailure{
				Matcher:    "rate_limit",
				At:         1714000000,
				ErrorShape: []byte(`{"type":"rate_limit_error"}`),
			},
			UsageProbe: &exhaustionproof.UsageProbe{
				At:     1714000010,
				Output: []byte(`limit hit`),
			},
			CapturedAt: 1714000020,
			Nonce:      []byte("proof-nonce-1234"), // 16 bytes
		},
		BalanceProof: &SignedBalanceSnapshot{
			Body: &BalanceSnapshotBody{
				IdentityId:   make32(0x33),
				Credits:      9999,
				ChainTipHash: make32(0x44),
				ChainTipSeq:  100,
				IssuedAt:     1714000000,
				ExpiresAt:    1714000600,
			},
			TrackerSig: make64(0x55),
		},
		CapturedAt: 1714000025,
		Nonce:      []byte("envelope-nonce12"), // 16 bytes
	}
}

func TestEnvelopeBody_RoundTrip(t *testing.T) {
	original := fixtureEnvelopeBody()

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(original)
	require.NoError(t, err)

	var parsed EnvelopeBody
	require.NoError(t, proto.Unmarshal(first, &parsed))

	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(&parsed)
	require.NoError(t, err)

	assert.Equal(t, first, second, "Deterministic marshal must be byte-stable")
	require.True(t, proto.Equal(original, &parsed), "unmarshal must reproduce original exactly")
}

func TestEnvelopeSigned_RoundTrip(t *testing.T) {
	signed := &EnvelopeSigned{
		Body:        fixtureEnvelopeBody(),
		ConsumerSig: make64(0x77),
	}

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(signed)
	require.NoError(t, err)

	var parsed EnvelopeSigned
	require.NoError(t, proto.Unmarshal(first, &parsed))

	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(&parsed)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	require.True(t, proto.Equal(signed, &parsed), "unmarshal must reproduce original exactly")
}

const goldenPath = "testdata/envelope_signed.golden.hex"

// goldenKeypair returns the deterministic Ed25519 keypair used to produce
// the golden fixture. Same seed as shared/signing tests so signatures
// reproduce across packages.
func goldenKeypair() (ed25519.PublicKey, ed25519.PrivateKey) {
	seed := []byte("token-bay-fixture-seed-v1-000000") // 32 bytes
	priv := ed25519.NewKeyFromSeed(seed)
	return priv.Public().(ed25519.PublicKey), priv
}

func TestEnvelopeSigned_GoldenBytes(t *testing.T) {
	body := fixtureEnvelopeBody()

	bodyBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	require.NoError(t, err)

	_, priv := goldenKeypair()
	sig := ed25519.Sign(priv, bodyBytes)

	signed := &EnvelopeSigned{Body: body, ConsumerSig: sig}
	wireBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(signed)
	require.NoError(t, err)

	gotHex := hex.EncodeToString(wireBytes)

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		require.NoError(t, os.MkdirAll(filepath.Dir(goldenPath), 0o755))
		require.NoError(t, os.WriteFile(goldenPath, []byte(gotHex+"\n"), 0o644))
		t.Logf("golden updated at %s", goldenPath)
		return
	}

	wantBytes, err := os.ReadFile(goldenPath)
	require.NoError(t, err, "missing golden file — run: UPDATE_GOLDEN=1 go test ./proto/... -run TestEnvelopeSigned_GoldenBytes")
	wantHex := strings.TrimSpace(string(wantBytes))

	assert.Equal(t, wantHex, gotHex,
		"EnvelopeSigned wire bytes differ from golden. If schema/lib intentionally changed, "+
			"regenerate via UPDATE_GOLDEN=1 and review the diff manually.")
}

package proto

import (
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

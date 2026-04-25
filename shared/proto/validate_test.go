package proto

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateEnvelopeBody_HappyPath(t *testing.T) {
	require.NoError(t, ValidateEnvelopeBody(fixtureEnvelopeBody()))
}

func TestValidateEnvelopeBody_Rejections(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(b *EnvelopeBody)
		errFrag string
	}{
		{"wrong protocol_version", func(b *EnvelopeBody) { b.ProtocolVersion = 99 }, "protocol_version"},
		{"consumer_id too short", func(b *EnvelopeBody) { b.ConsumerId = []byte{1, 2, 3} }, "consumer_id"},
		{"consumer_id too long", func(b *EnvelopeBody) { b.ConsumerId = make([]byte, 64) }, "consumer_id"},
		{"empty model", func(b *EnvelopeBody) { b.Model = "" }, "model"},
		{"body_hash wrong length", func(b *EnvelopeBody) { b.BodyHash = []byte{1, 2} }, "body_hash"},
		{"missing exhaustion_proof", func(b *EnvelopeBody) { b.ExhaustionProof = nil }, "exhaustion_proof"},
		{"missing balance_proof", func(b *EnvelopeBody) { b.BalanceProof = nil }, "balance_proof"},
		{"zero captured_at", func(b *EnvelopeBody) { b.CapturedAt = 0 }, "captured_at"},
		{"nonce too short", func(b *EnvelopeBody) { b.Nonce = []byte("short") }, "nonce"},
		{"unspecified tier", func(b *EnvelopeBody) { b.Tier = PrivacyTier_PRIVACY_TIER_UNSPECIFIED }, "tier"},
		{"unknown tier value", func(b *EnvelopeBody) { b.Tier = PrivacyTier(99) }, "tier"},
		{"invalid exhaustion_proof", func(b *EnvelopeBody) { b.ExhaustionProof.StopFailure.Matcher = "wrong" }, "exhaustion_proof"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := fixtureEnvelopeBody()
			tc.mutate(b)
			err := ValidateEnvelopeBody(b)
			require.Error(t, err, "case %q should fail validation", tc.name)
			assert.Contains(t, err.Error(), tc.errFrag, "error should mention %q", tc.errFrag)
		})
	}
}

func TestValidateEnvelopeBody_NilBody(t *testing.T) {
	err := ValidateEnvelopeBody(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

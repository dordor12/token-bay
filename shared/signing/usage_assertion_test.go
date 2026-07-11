package signing

import (
	"bytes"
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUsageAssertion_SignVerify_RoundTrip(t *testing.T) {
	seed := bytes.Repeat([]byte{9}, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	a := UsageAssertion{
		RequestID: bytes.Repeat([]byte{1}, 16), ConsumerID: bytes.Repeat([]byte{2}, 32),
		SeederID: bytes.Repeat([]byte{3}, 32), Model: "claude-sonnet-4-6",
		InputTokens: 100, OutputTokens: 50, CostCredits: 1500,
	}
	sig, err := SignUsageAssertion(priv, a)
	require.NoError(t, err)
	require.True(t, VerifyUsageAssertion(priv.Public().(ed25519.PublicKey), a, sig))
	a.OutputTokens = 51 // tamper
	require.False(t, VerifyUsageAssertion(priv.Public().(ed25519.PublicKey), a, sig))
}

func TestCanonicalUsageAssertion_Deterministic(t *testing.T) {
	a := UsageAssertion{
		RequestID: bytes.Repeat([]byte{1}, 16), ConsumerID: bytes.Repeat([]byte{2}, 32),
		SeederID: bytes.Repeat([]byte{3}, 32), Model: "m", InputTokens: 1, OutputTokens: 2, CostCredits: 3,
	}
	b1, err := CanonicalUsageAssertionPreSig(a)
	require.NoError(t, err)
	b2, _ := CanonicalUsageAssertionPreSig(a)
	require.Equal(t, b1, b2)
	require.NotEmpty(t, b1)
}

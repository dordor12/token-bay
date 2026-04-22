package signing

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerify_ValidSignature_ReturnsTrue(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	msg := []byte("token-bay test")
	sig := ed25519.Sign(priv, msg)

	assert.True(t, Verify(pub, msg, sig))
}

func TestVerify_TamperedMessage_ReturnsFalse(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	msg := []byte("token-bay test")
	sig := ed25519.Sign(priv, msg)
	tampered := []byte("token-bay xest")

	assert.False(t, Verify(pub, tampered, sig))
}

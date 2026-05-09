package identity

import (
	"crypto/ed25519"
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_RejectsBadKeyLength(t *testing.T) {
	_, err := New(ed25519.PrivateKey{1, 2, 3})
	require.ErrorIs(t, err, ErrInvalidKey)
}

func TestGenerate_PopulatesIdentityID(t *testing.T) {
	s, err := Generate()
	require.NoError(t, err)
	require.Len(t, s.PrivateKey(), ed25519.PrivateKeySize)
	require.Len(t, s.PublicKey(), ed25519.PublicKeySize)

	expectedID := sha256.Sum256(s.PublicKey())
	assert.Equal(t, expectedID, [32]byte(s.IdentityID()))
}

func TestSign_RoundTripVerify(t *testing.T) {
	s, err := Generate()
	require.NoError(t, err)

	msg := []byte("hello identity")
	sig, err := s.Sign(msg)
	require.NoError(t, err)
	require.Len(t, sig, ed25519.SignatureSize)

	assert.True(t, ed25519.Verify(s.PublicKey(), msg, sig))
}

func TestNew_FromGeneratedKey_PreservesIdentityID(t *testing.T) {
	s, err := Generate()
	require.NoError(t, err)
	s2, err := New(s.PrivateKey())
	require.NoError(t, err)
	assert.Equal(t, s.IdentityID(), s2.IdentityID())
}

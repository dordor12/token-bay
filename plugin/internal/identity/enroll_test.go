package identity

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnrollPreimage_LayoutAndLength(t *testing.T) {
	var nonce [16]byte
	for i := range nonce {
		nonce[i] = byte(i + 1)
	}
	var fp [32]byte
	for i := range fp {
		fp[i] = 0xAA
	}
	pub := make(ed25519.PublicKey, ed25519.PublicKeySize)
	for i := range pub {
		pub[i] = 0xBB
	}

	pre, err := EnrollPreimage(nonce, fp, pub)
	require.NoError(t, err)
	assert.Len(t, pre, len(EnrollPreimagePrefix)+16+32+32)
	assert.Equal(t, 99, len(pre))

	assert.True(t, bytes.HasPrefix(pre, []byte(EnrollPreimagePrefix)))
	off := len(EnrollPreimagePrefix)
	assert.Equal(t, nonce[:], pre[off:off+16])
	assert.Equal(t, fp[:], pre[off+16:off+16+32])
	assert.Equal(t, []byte(pub), pre[off+16+32:])
}

func TestEnrollPreimage_KnownHexVector(t *testing.T) {
	var nonce [16]byte
	var fp [32]byte
	pub := make(ed25519.PublicKey, ed25519.PublicKeySize)
	pre, err := EnrollPreimage(nonce, fp, pub)
	require.NoError(t, err)

	// 19 bytes prefix ("token-bay-enroll:v1") + 80 bytes of zeros.
	expected := append([]byte(EnrollPreimagePrefix), make([]byte, 80)...)
	assert.Equal(t, hex.EncodeToString(expected), hex.EncodeToString(pre))
}

func TestEnrollPreimage_RejectsBadPubKeyLength(t *testing.T) {
	_, err := EnrollPreimage([16]byte{}, [32]byte{}, ed25519.PublicKey{1, 2, 3})
	require.ErrorIs(t, err, ErrInvalidKey)
}

func TestBuildEnrollPayload_SignatureVerifies(t *testing.T) {
	s, err := Generate()
	require.NoError(t, err)

	var fp [32]byte
	for i := range fp {
		fp[i] = 0xCD
	}

	p, err := BuildEnrollPayload(s, fp, RoleConsumer|RoleSeeder)
	require.NoError(t, err)
	assert.Equal(t, []byte(s.PublicKey()), []byte(p.IdentityPubkey))
	assert.Equal(t, fp, p.AccountFingerprint)
	assert.Equal(t, RoleConsumer|RoleSeeder, p.Role)
	assert.Len(t, p.Sig, ed25519.SignatureSize)

	pre, err := EnrollPreimage(p.Nonce, p.AccountFingerprint, p.IdentityPubkey)
	require.NoError(t, err)
	assert.True(t, ed25519.Verify(s.PublicKey(), pre, p.Sig))
}

func TestBuildEnrollPayload_NonceIsRandom(t *testing.T) {
	s, err := Generate()
	require.NoError(t, err)

	var fp [32]byte
	p1, err := BuildEnrollPayload(s, fp, RoleConsumer)
	require.NoError(t, err)
	p2, err := BuildEnrollPayload(s, fp, RoleConsumer)
	require.NoError(t, err)
	assert.NotEqual(t, p1.Nonce, p2.Nonce, "nonce should be random per call")
}

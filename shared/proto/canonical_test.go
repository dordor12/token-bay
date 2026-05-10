package proto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return pub, priv
}

func TestCanonicalBootstrapPeerListPreSig_ExcludesSig(t *testing.T) {
	bl := validBootstrapPeerList()
	a, err := CanonicalBootstrapPeerListPreSig(bl)
	require.NoError(t, err)

	bl.Sig = bytes.Repeat([]byte{0xFF}, ed25519.SignatureSize)
	b, err := CanonicalBootstrapPeerListPreSig(bl)
	require.NoError(t, err)

	require.Equal(t, a, b, "canonical bytes must not depend on Sig")
}

func TestCanonicalBootstrapPeerListPreSig_Stable(t *testing.T) {
	bl1 := validBootstrapPeerList()
	bl2 := validBootstrapPeerList()
	a, err := CanonicalBootstrapPeerListPreSig(bl1)
	require.NoError(t, err)
	b, err := CanonicalBootstrapPeerListPreSig(bl2)
	require.NoError(t, err)
	require.Equal(t, a, b)
}

func TestCanonicalBootstrapPeerListPreSig_NilRejected(t *testing.T) {
	_, err := CanonicalBootstrapPeerListPreSig(nil)
	require.Error(t, err)
}

func TestSignAndVerifyBootstrapPeerList_RoundTrip(t *testing.T) {
	pub, priv := newKey(t)
	bl := validBootstrapPeerList()
	bl.Sig = nil
	require.NoError(t, SignBootstrapPeerList(priv, bl))
	require.Len(t, bl.Sig, ed25519.SignatureSize)
	require.NoError(t, VerifyBootstrapPeerListSig(pub, bl))
}

func TestVerifyBootstrapPeerListSig_RejectsTampered(t *testing.T) {
	pub, priv := newKey(t)
	bl := validBootstrapPeerList()
	bl.Sig = nil
	require.NoError(t, SignBootstrapPeerList(priv, bl))
	bl.Peers[0].Addr = "tampered.example.org:443"
	err := VerifyBootstrapPeerListSig(pub, bl)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrBootstrapPeerListBadSig))
}

func TestVerifyBootstrapPeerListSig_RejectsWrongPubkey(t *testing.T) {
	_, priv := newKey(t)
	otherPub, _ := newKey(t)
	bl := validBootstrapPeerList()
	bl.Sig = nil
	require.NoError(t, SignBootstrapPeerList(priv, bl))
	err := VerifyBootstrapPeerListSig(otherPub, bl)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrBootstrapPeerListBadSig))
}

func TestVerifyBootstrapPeerListSig_RejectsBadPubkey(t *testing.T) {
	_, priv := newKey(t)
	bl := validBootstrapPeerList()
	bl.Sig = nil
	require.NoError(t, SignBootstrapPeerList(priv, bl))
	require.Error(t, VerifyBootstrapPeerListSig(ed25519.PublicKey{1, 2, 3}, bl))
}

package idtls

import (
	"crypto/ed25519"
	"crypto/x509"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCertFromIdentityProducesParseableCert(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	cert, err := CertFromIdentity(priv)
	require.NoError(t, err)
	require.Len(t, cert.Certificate, 1)

	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	require.NoError(t, err)

	pub, ok := parsed.PublicKey.(ed25519.PublicKey)
	require.True(t, ok)
	assert.True(t, pub.Equal(priv.Public()))
}

func TestCertFromIdentityRejectsBadKey(t *testing.T) {
	_, err := CertFromIdentity(ed25519.PrivateKey{1, 2, 3})
	require.Error(t, err)
}

func TestSPKIHashStableAcrossRegeneration(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	c1, err := CertFromIdentity(priv)
	require.NoError(t, err)
	p1, err := x509.ParseCertificate(c1.Certificate[0])
	require.NoError(t, err)
	h1, err := SPKIHashOfCert(p1)
	require.NoError(t, err)

	c2, err := CertFromIdentity(priv)
	require.NoError(t, err)
	p2, err := x509.ParseCertificate(c2.Certificate[0])
	require.NoError(t, err)
	h2, err := SPKIHashOfCert(p2)
	require.NoError(t, err)

	assert.Equal(t, h1, h2, "SPKI hash must be stable for the same keypair")
}

func TestSPKIHashNilSafe(t *testing.T) {
	_, err := SPKIHashOfCert(nil)
	require.Error(t, err)
}

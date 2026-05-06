package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedClock returns an injected clock for cert NotBefore/NotAfter.
func fixedClock(t *testing.T) func() time.Time {
	t.Helper()
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return now }
}

func TestSelfSignedCert_RoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	cert, err := selfSignedCert(priv, fixedClock(t)())
	require.NoError(t, err)
	require.NotNil(t, cert)
	require.Len(t, cert.Certificate, 1)

	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	require.NoError(t, err)

	got, ok := parsed.PublicKey.(ed25519.PublicKey)
	require.True(t, ok, "leaf pubkey not Ed25519")
	assert.Equal(t, ed25519.PublicKey(pub), got)
}

func TestSelfSignedCert_UsesInjectedClock(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	cert, err := selfSignedCert(priv, now)
	require.NoError(t, err)

	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	require.NoError(t, err)

	assert.True(t, parsed.NotBefore.Before(now) || parsed.NotBefore.Equal(now))
	assert.True(t, parsed.NotAfter.After(now))
	assert.WithinDuration(t, now.Add(time.Hour), parsed.NotAfter, 5*time.Minute)
}

func TestVerifyPeerPin_Match(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	cert, err := selfSignedCert(priv, fixedClock(t)())
	require.NoError(t, err)

	verifier := verifyPeerPin(pub)
	err = verifier([][]byte{cert.Certificate[0]}, nil)
	assert.NoError(t, err)
}

func TestVerifyPeerPin_Mismatch(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	cert, err := selfSignedCert(priv, fixedClock(t)())
	require.NoError(t, err)

	verifier := verifyPeerPin(otherPub)
	err = verifier([][]byte{cert.Certificate[0]}, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrPeerPinMismatch))
}

func TestVerifyPeerPin_NoCertChain(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	verifier := verifyPeerPin(pub)
	err = verifier(nil, nil)
	assert.True(t, errors.Is(err, ErrPeerPinMismatch))
}

func TestVerifyPeerPin_NonEd25519Cert(t *testing.T) {
	// crypto/x509 self-signed RSA cert (synthesized cheaply via crypto/tls).
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	// Pass garbage cert bytes; ParseCertificate will fail and we expect ErrPeerPinMismatch.
	verifier := verifyPeerPin(pub)
	err = verifier([][]byte{[]byte("not-a-cert")}, nil)
	assert.True(t, errors.Is(err, ErrPeerPinMismatch))
}

func TestVerifyPeerPin_ExpectedSizeMismatch(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	cert, err := selfSignedCert(priv, fixedClock(t)())
	require.NoError(t, err)

	// expected pubkey is wrong-length (zero-len), triggers size guard
	verifier := verifyPeerPin(ed25519.PublicKey{})
	err = verifier([][]byte{cert.Certificate[0]}, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrPeerPinMismatch))
}

func TestNewTLSConfig_DialerSide(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	peerPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	cfg, err := newTLSConfig(priv, peerPub, fixedClock(t)(), false /* listener */)
	require.NoError(t, err)
	require.NotNil(t, cfg)

	assert.Equal(t, []string{alpnProto}, cfg.NextProtos)
	assert.True(t, cfg.InsecureSkipVerify)
	assert.NotNil(t, cfg.VerifyPeerCertificate)
	assert.Len(t, cfg.Certificates, 1)
	_ = pub
}

func TestNewTLSConfig_ListenerSide(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	peerPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	cfg, err := newTLSConfig(priv, peerPub, fixedClock(t)(), true /* listener */)
	require.NoError(t, err)
	assert.Equal(t, tls.RequireAnyClientCert, cfg.ClientAuth)
}

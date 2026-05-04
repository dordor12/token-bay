package idtls

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustSelfSignedRSA(t *testing.T) []byte {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "rsa-imposter"},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)
	return der
}

func TestPinVerifierAccepts(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	cert, err := CertFromIdentity(priv)
	require.NoError(t, err)
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	require.NoError(t, err)
	pin, err := SPKIHashOfCert(parsed)
	require.NoError(t, err)

	err = pinVerifier(pin)([][]byte{cert.Certificate[0]}, nil)
	assert.NoError(t, err)
}

func TestPinVerifierRejectsMismatch(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	cert, err := CertFromIdentity(priv)
	require.NoError(t, err)

	err = pinVerifier([32]byte{0xff})([][]byte{cert.Certificate[0]}, nil)
	assert.ErrorIs(t, err, ErrIdentityMismatch)
}

func TestPinVerifierRejectsRSA(t *testing.T) {
	der := mustSelfSignedRSA(t)
	err := pinVerifier([32]byte{0xff})([][]byte{der}, nil)
	assert.ErrorIs(t, err, ErrNotEd25519)
}

func TestPinVerifierRejectsEmptyChain(t *testing.T) {
	err := pinVerifier([32]byte{})([][]byte{}, nil)
	assert.ErrorIs(t, err, ErrNoPeerCert)
}

func TestPinVerifierRejectsMultiCert(t *testing.T) {
	err := pinVerifier([32]byte{})([][]byte{{1}, {2}}, nil)
	assert.ErrorIs(t, err, ErrTooManyCerts)
}

func TestServerVerifierCapturesHash(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	cert, err := CertFromIdentity(priv)
	require.NoError(t, err)

	var got [32]byte
	err = serverVerifier(func(h [32]byte) { got = h })([][]byte{cert.Certificate[0]}, nil)
	require.NoError(t, err)
	assert.NotEqual(t, [32]byte{}, got)
}

func TestMakeClientTLSConfigShape(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	cert, _ := CertFromIdentity(priv)
	cfg := MakeClientTLSConfig(cert, [32]byte{1})
	assert.True(t, cfg.InsecureSkipVerify)
	assert.True(t, cfg.SessionTicketsDisabled)
	assert.Equal(t, []string{ALPN}, cfg.NextProtos)
	assert.Equal(t, uint16(0x0304), cfg.MinVersion) // TLS 1.3
	assert.NotNil(t, cfg.VerifyPeerCertificate)
}

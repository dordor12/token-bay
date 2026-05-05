package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"time"
)

// alpnProto is the single ALPN id this tunnel speaks.
const alpnProto = "tb-tun/1"

// certValidity is the NotAfter offset for self-signed leaf certs.
const certValidity = time.Hour

// certSkew is the NotBefore back-dating window to absorb clock skew.
const certSkew = 5 * time.Minute

// selfSignedCert produces a one-cert tls.Certificate self-signed by
// priv. Subject CN is the hex-encoded Ed25519 pubkey for trace
// readability; pinning ignores it.
func selfSignedCert(priv ed25519.PrivateKey, now time.Time) (*tls.Certificate, error) {
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%w: priv.Public() is not Ed25519", ErrInvalidConfig)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("%w: serial: %v", ErrInvalidConfig, err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: fmt.Sprintf("tb-tun/%x", pub[:8])},
		NotBefore:    now.Add(-certSkew),
		NotAfter:     now.Add(certValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, fmt.Errorf("%w: create: %v", ErrInvalidConfig, err)
	}
	return &tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  priv,
	}, nil
}

// verifyPeerPin returns a tls.Config.VerifyPeerCertificate callback
// that succeeds iff the peer's leaf cert presents an Ed25519
// SubjectPublicKeyInfo equal to expected.
func verifyPeerPin(expected ed25519.PublicKey) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("%w: peer presented no certificates", ErrPeerPinMismatch)
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("%w: parse leaf: %v", ErrPeerPinMismatch, err)
		}
		got, ok := leaf.PublicKey.(ed25519.PublicKey)
		if !ok {
			return fmt.Errorf("%w: leaf pubkey is not Ed25519", ErrPeerPinMismatch)
		}
		if len(got) != ed25519.PublicKeySize || len(expected) != ed25519.PublicKeySize {
			return fmt.Errorf("%w: pubkey size mismatch", ErrPeerPinMismatch)
		}
		// constant-time-ish equality; len-checked above
		var diff byte
		for i := 0; i < ed25519.PublicKeySize; i++ {
			diff |= got[i] ^ expected[i]
		}
		if diff != 0 {
			return fmt.Errorf("%w", ErrPeerPinMismatch)
		}
		return nil
	}
}

// newTLSConfig builds a *tls.Config for either side. listener=true
// switches to the server-side template (RequireAnyClientCert).
func newTLSConfig(priv ed25519.PrivateKey, peerPub ed25519.PublicKey, now time.Time, listener bool) (*tls.Config, error) {
	cert, err := selfSignedCert(priv, now)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		Certificates:          []tls.Certificate{*cert},
		NextProtos:            []string{alpnProto},
		MinVersion:            tls.VersionTLS13,
		MaxVersion:            tls.VersionTLS13,
		InsecureSkipVerify:    true, //nolint:gosec // VerifyPeerCertificate replaces CA validation; pinning is the auth surface
		VerifyPeerCertificate: verifyPeerPin(peerPub),
	}
	if listener {
		cfg.ClientAuth = tls.RequireAnyClientCert
	}
	return cfg, nil
}

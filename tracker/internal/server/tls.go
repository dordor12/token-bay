package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// ALPN is the protocol identifier negotiated on every Token-Bay
// tracker↔plugin TLS link. Mirror of the merged plugin
// internal/idtls.ALPN — both ends MUST agree.
const ALPN = "tokenbay/1"

// CertFromIdentity issues a self-signed X.509 cert wrapping priv.
// The cert is generated deterministically (modulo the CSPRNG bytes that
// X.509 sprinkles into ephemeral places) from the keypair; safe to
// regenerate per process start.
func CertFromIdentity(priv ed25519.PrivateKey) (tls.Certificate, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return tls.Certificate{}, fmt.Errorf("server: priv length %d, want %d",
			len(priv), ed25519.PrivateKeySize)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "token-bay-tracker"},
		NotBefore:             time.Unix(0, 0),
		NotAfter:              time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, priv.Public(), priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("server: CreateCertificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}

// SPKIToIdentityID hashes the cert's SubjectPublicKeyInfo bytes —
// the canonical IdentityID for the cert's holder. Empty SPKI is
// rejected to defeat hand-crafted truncated certs.
func SPKIToIdentityID(cert *x509.Certificate) (ids.IdentityID, error) {
	if cert == nil {
		return ids.IdentityID{}, errors.New("server: nil cert")
	}
	if len(cert.RawSubjectPublicKeyInfo) == 0 {
		return ids.IdentityID{}, errors.New("server: empty SPKI")
	}
	var id ids.IdentityID
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	copy(id[:], sum[:])
	return id, nil
}

// VerifyClientCert is the VerifyPeerCertificate hook for the server's
// TLS config. Accepts exactly one Ed25519 client cert with non-empty
// SPKI. Web-PKI validity is intentionally NOT checked — the SPKI pin
// is the canonical identity check.
func VerifyClientCert(rawCerts [][]byte) error {
	if len(rawCerts) == 0 {
		return errors.New("server: no peer certificate")
	}
	if len(rawCerts) > 1 {
		return errors.New("server: peer offered more than one cert")
	}
	parsed, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return fmt.Errorf("server: parse peer cert: %w", err)
	}
	if _, ok := parsed.PublicKey.(ed25519.PublicKey); !ok {
		return errors.New("server: peer cert is not Ed25519")
	}
	if len(parsed.RawSubjectPublicKeyInfo) == 0 {
		return errors.New("server: empty SPKI")
	}
	return nil
}

// MakeServerTLSConfig builds the tls.Config used by the QUIC listener.
// Mutual auth: requires any client cert and validates via
// VerifyClientCert. ALPN locks "tokenbay/1"; TLS 1.3 only;
// session tickets disabled (no 0-RTT).
func MakeServerTLSConfig(ourCert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates:       []tls.Certificate{ourCert},
		ClientAuth:         tls.RequireAnyClientCert,
		InsecureSkipVerify: true, //nolint:gosec // see VerifyPeerCertificate
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return VerifyClientCert(rawCerts)
		},
		NextProtos:             []string{ALPN},
		MinVersion:             tls.VersionTLS13,
		SessionTicketsDisabled: true,
	}
}

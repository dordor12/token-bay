// Package idtls binds the plugin's Ed25519 identity keypair to a TLS
// session. Generates a self-signed X.509 cert whose SubjectPublicKeyInfo
// carries the Ed25519 pubkey; the SPKI hash IS the IdentityID.
package idtls

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
)

// ALPN is the protocol identifier negotiated on every Token-Bay TLS link.
// v2 will ship as "tokenbay/2"; old/new servers can co-exist.
const ALPN = "tokenbay/1"

// CertFromIdentity issues a self-signed X.509 cert wrapping priv.
// The cert is generated deterministically from the keypair and is safe to
// regenerate on each call (we cache it at the Client level for efficiency).
func CertFromIdentity(priv ed25519.PrivateKey) (tls.Certificate, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return tls.Certificate{}, fmt.Errorf("idtls: priv length %d, want %d", len(priv), ed25519.PrivateKeySize)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "token-bay-identity"},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth,
			x509.ExtKeyUsageServerAuth,
		},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, priv.Public(), priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("idtls: CreateCertificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}

// SPKIHashOfCert extracts the SHA-256 of the cert's SubjectPublicKeyInfo
// bytes — the canonical IdentityID for the cert's holder.
func SPKIHashOfCert(cert *x509.Certificate) ([32]byte, error) {
	if cert == nil {
		return [32]byte{}, errors.New("idtls: nil cert")
	}
	if len(cert.RawSubjectPublicKeyInfo) == 0 {
		return [32]byte{}, errors.New("idtls: empty SPKI")
	}
	return sha256.Sum256(cert.RawSubjectPublicKeyInfo), nil
}

package federation

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

// ALPN is the protocol identifier negotiated on every Token-Bay
// federation peering link. Distinct from the plugin-facing
// "tokenbay/1" so a misconfigured operator pointing federation at the
// plugin port cannot accidentally cross-serve.
const ALPN = "tokenbay-fed/1"

// Cert + verifier sentinel errors.
var (
	ErrNoPeerCert   = errors.New("federation: no peer certificate")
	ErrTooManyCerts = errors.New("federation: peer offered more than one cert")
	ErrNotEd25519   = errors.New("federation: peer cert is not Ed25519")
	ErrSPKIMismatch = errors.New("federation: peer cert SPKI does not match pin")
	ErrEmptySPKI    = errors.New("federation: empty SPKI")
)

// CertFromIdentity issues a self-signed X.509 cert wrapping priv. The
// cert is generated deterministically from the keypair (modulo CSPRNG
// bytes that x509 sprinkles in) and is safe to regenerate per process
// start. The resulting tls.Certificate carries priv as its PrivateKey.
func CertFromIdentity(priv ed25519.PrivateKey) (tls.Certificate, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return tls.Certificate{}, fmt.Errorf("federation: priv length %d, want %d",
			len(priv), ed25519.PrivateKeySize)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "token-bay-federation"},
		NotBefore:             time.Unix(0, 0),
		NotAfter:              time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, priv.Public(), priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("federation: CreateCertificate: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}

// SPKIHashOfCert returns the SHA-256 of the cert's
// SubjectPublicKeyInfo bytes — the canonical pin used for federation
// peer identification. Empty SPKI is rejected to defeat hand-crafted
// truncated certs.
func SPKIHashOfCert(cert *x509.Certificate) ([32]byte, error) {
	if cert == nil {
		return [32]byte{}, errors.New("federation: nil cert")
	}
	if len(cert.RawSubjectPublicKeyInfo) == 0 {
		return [32]byte{}, ErrEmptySPKI
	}
	return sha256.Sum256(cert.RawSubjectPublicKeyInfo), nil
}

// MakeClientTLSConfig is used by the dialer side. Pins the server's SPKI
// hash; rejects multi-cert chains, non-Ed25519, and SPKI mismatches.
func MakeClientTLSConfig(ourCert tls.Certificate, expectedServerSPKI [32]byte) *tls.Config {
	return &tls.Config{
		Certificates:           []tls.Certificate{ourCert},
		InsecureSkipVerify:     true, //nolint:gosec // see VerifyPeerCertificate
		VerifyPeerCertificate:  pinVerifier(expectedServerSPKI),
		NextProtos:             []string{ALPN},
		MinVersion:             tls.VersionTLS13,
		SessionTicketsDisabled: true,
	}
}

// MakeServerTLSConfig is used by the listener side. Accepts any client
// whose cert is a valid self-signed Ed25519 cert; the SPKI hash is
// captured via the supplied callback so the accept handler can verify
// it against the operator allowlist.
func MakeServerTLSConfig(ourCert tls.Certificate, captureClientHash func([32]byte)) *tls.Config {
	return &tls.Config{
		Certificates:           []tls.Certificate{ourCert},
		ClientAuth:             tls.RequireAnyClientCert,
		InsecureSkipVerify:     true, //nolint:gosec // see VerifyPeerCertificate
		VerifyPeerCertificate:  serverVerifier(captureClientHash),
		NextProtos:             []string{ALPN},
		MinVersion:             tls.VersionTLS13,
		SessionTicketsDisabled: true,
	}
}

func pinVerifier(expected [32]byte) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return ErrNoPeerCert
		}
		if len(rawCerts) > 1 {
			return ErrTooManyCerts
		}
		peer, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("federation: parse peer cert: %w", err)
		}
		if _, ok := peer.PublicKey.(ed25519.PublicKey); !ok {
			return ErrNotEd25519
		}
		got, err := SPKIHashOfCert(peer)
		if err != nil {
			return err
		}
		if got != expected {
			return ErrSPKIMismatch
		}
		return nil
	}
}

func serverVerifier(capture func([32]byte)) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return ErrNoPeerCert
		}
		if len(rawCerts) > 1 {
			return ErrTooManyCerts
		}
		peer, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("federation: parse peer cert: %w", err)
		}
		if _, ok := peer.PublicKey.(ed25519.PublicKey); !ok {
			return ErrNotEd25519
		}
		h, err := SPKIHashOfCert(peer)
		if err != nil {
			return err
		}
		if capture != nil {
			capture(h)
		}
		return nil
	}
}

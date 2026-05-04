package idtls

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
)

var (
	ErrNoPeerCert       = errors.New("idtls: no peer certificate")
	ErrTooManyCerts     = errors.New("idtls: peer offered more than one cert")
	ErrNotEd25519       = errors.New("idtls: peer cert is not Ed25519")
	ErrIdentityMismatch = errors.New("idtls: peer identity does not match pin")
)

// MakeClientTLSConfig returns a *tls.Config wired for our mTLS contract:
//   - Presents ourCert (the local identity-bound cert).
//   - Disables Web-PKI verification.
//   - Pins the server's SPKI to expectedServerHash via VerifyPeerCertificate.
//   - Negotiates ALPN "tokenbay/1".
//   - Disables 0-RTT (SessionTicketsDisabled).
func MakeClientTLSConfig(ourCert tls.Certificate, expectedServerHash [32]byte) *tls.Config {
	return &tls.Config{
		Certificates:           []tls.Certificate{ourCert},
		InsecureSkipVerify:     true, //nolint:gosec // see VerifyPeerCertificate
		VerifyPeerCertificate:  pinVerifier(expectedServerHash),
		NextProtos:             []string{ALPN},
		MinVersion:             tls.VersionTLS13,
		SessionTicketsDisabled: true,
	}
}

// MakeServerTLSConfig is the symmetric helper for the in-process fakeserver
// and the real tracker (eventually). It accepts any client whose cert is a
// valid self-signed Ed25519 cert, reporting the SPKI hash via the helper
// captureClientHash.
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
			return fmt.Errorf("idtls: parse peer cert: %w", err)
		}
		if _, ok := peer.PublicKey.(ed25519.PublicKey); !ok {
			return ErrNotEd25519
		}
		got, err := SPKIHashOfCert(peer)
		if err != nil {
			return err
		}
		if got != expected {
			return ErrIdentityMismatch
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
			return fmt.Errorf("idtls: parse peer cert: %w", err)
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

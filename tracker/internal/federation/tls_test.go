package federation_test

import (
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"strings"
	"testing"

	"github.com/token-bay/token-bay/tracker/internal/federation"
)

func TestCertFromIdentity_RoundTrip(t *testing.T) {
	t.Parallel()
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	cert, err := federation.CertFromIdentity(priv)
	if err != nil {
		t.Fatalf("CertFromIdentity: %v", err)
	}
	if len(cert.Certificate) != 1 {
		t.Fatalf("expected 1 cert, got %d", len(cert.Certificate))
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	gotPub, ok := parsed.PublicKey.(ed25519.PublicKey)
	if !ok {
		t.Fatal("cert pubkey is not Ed25519")
	}
	if string(gotPub) != string(pub) {
		t.Fatal("cert pubkey != input pubkey")
	}
}

func TestCertFromIdentity_RejectsBadKeyLength(t *testing.T) {
	t.Parallel()
	if _, err := federation.CertFromIdentity(make(ed25519.PrivateKey, 1)); err == nil {
		t.Fatal("expected error for short key")
	}
}

func TestSPKIHashOfCert_Deterministic(t *testing.T) {
	t.Parallel()
	_, priv, _ := ed25519.GenerateKey(crand.Reader)
	cert, _ := federation.CertFromIdentity(priv)
	parsed, _ := x509.ParseCertificate(cert.Certificate[0])
	h1, err := federation.SPKIHashOfCert(parsed)
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := federation.SPKIHashOfCert(parsed)
	if h1 != h2 {
		t.Fatal("SPKI hash differs across calls on same cert")
	}
	expect := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
	if h1 != expect {
		t.Fatal("SPKI hash != sha256(RawSubjectPublicKeyInfo)")
	}
}

func TestSPKIHashOfCert_RejectsNilOrEmpty(t *testing.T) {
	t.Parallel()
	if _, err := federation.SPKIHashOfCert(nil); err == nil {
		t.Fatal("expected error on nil cert")
	}
	if _, err := federation.SPKIHashOfCert(&x509.Certificate{}); err == nil {
		t.Fatal("expected error on empty SPKI")
	}
}

func TestPinVerifier_RejectsCases(t *testing.T) {
	t.Parallel()
	_, priv, _ := ed25519.GenerateKey(crand.Reader)
	cert, _ := federation.CertFromIdentity(priv)
	parsed, _ := x509.ParseCertificate(cert.Certificate[0])
	expected, _ := federation.SPKIHashOfCert(parsed)

	pinned := federation.MakeClientTLSConfig(cert, expected)
	if err := pinned.VerifyPeerCertificate([][]byte{parsed.Raw}, nil); err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if err := pinned.VerifyPeerCertificate([][]byte{}, nil); !errors.Is(err, federation.ErrNoPeerCert) {
		t.Fatalf("expected ErrNoPeerCert, got %v", err)
	}
	if err := pinned.VerifyPeerCertificate([][]byte{parsed.Raw, parsed.Raw}, nil); !errors.Is(err, federation.ErrTooManyCerts) {
		t.Fatalf("expected ErrTooManyCerts, got %v", err)
	}
	_, otherPriv, _ := ed25519.GenerateKey(crand.Reader)
	otherCert, _ := federation.CertFromIdentity(otherPriv)
	if err := pinned.VerifyPeerCertificate([][]byte{otherCert.Certificate[0]}, nil); !errors.Is(err, federation.ErrSPKIMismatch) {
		t.Fatalf("expected ErrSPKIMismatch, got %v", err)
	}
	if err := pinned.VerifyPeerCertificate([][]byte{[]byte("not-a-cert")}, nil); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestServerTLSConfig_CapturesClientHash(t *testing.T) {
	t.Parallel()
	_, priv, _ := ed25519.GenerateKey(crand.Reader)
	srvCert, _ := federation.CertFromIdentity(priv)
	var captured [32]byte
	cfg := federation.MakeServerTLSConfig(srvCert, func(h [32]byte) { captured = h })

	_, cliPriv, _ := ed25519.GenerateKey(crand.Reader)
	cliCert, _ := federation.CertFromIdentity(cliPriv)
	parsed, _ := x509.ParseCertificate(cliCert.Certificate[0])
	if err := cfg.VerifyPeerCertificate([][]byte{parsed.Raw}, nil); err != nil {
		t.Fatalf("server verify: %v", err)
	}
	want, _ := federation.SPKIHashOfCert(parsed)
	if captured != want {
		t.Fatal("server verifier did not capture client SPKI hash")
	}
}

func TestALPN_DistinctFromPluginFacing(t *testing.T) {
	t.Parallel()
	if federation.ALPN == "" {
		t.Fatal("federation.ALPN is empty")
	}
	if federation.ALPN == "tokenbay/1" {
		t.Fatal("federation ALPN must not collide with plugin-facing ALPN")
	}
	if !strings.HasPrefix(federation.ALPN, "tokenbay-fed/") {
		t.Fatalf("federation ALPN should start with tokenbay-fed/, got %q", federation.ALPN)
	}
	cfg := federation.MakeServerTLSConfig(tls.Certificate{}, nil)
	if len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != federation.ALPN {
		t.Fatalf("server TLS config NextProtos = %v, want [%q]", cfg.NextProtos, federation.ALPN)
	}
}

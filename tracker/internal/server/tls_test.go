package server_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/token-bay/token-bay/tracker/internal/server"
)

func loadKey(t *testing.T, name string) ed25519.PrivateKey {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+"_ed25519.key"))
	if err != nil {
		t.Fatal(err)
	}
	return ed25519.PrivateKey(b)
}

func TestServerCertFromIdentity_OK(t *testing.T) {
	priv := loadKey(t, "server")
	cert, err := server.ServerCertFromIdentity(priv)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.Certificate) != 1 {
		t.Fatalf("want 1 cert; got %d", len(cert.Certificate))
	}
}

func TestServerCertFromIdentity_RejectsBadKeyLength(t *testing.T) {
	_, err := server.ServerCertFromIdentity(ed25519.PrivateKey{0x01})
	if err == nil {
		t.Fatal("want error on short key")
	}
}

func TestSPKIToIdentityID_RoundTrip(t *testing.T) {
	priv := loadKey(t, "server")
	cert, _ := server.ServerCertFromIdentity(priv)
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	id, err := server.SPKIToIdentityID(parsed)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
	if id != want {
		t.Fatalf("id = %x, want %x", id, want)
	}
}

func TestSPKIToIdentityID_RejectsNil(t *testing.T) {
	_, err := server.SPKIToIdentityID(nil)
	if err == nil {
		t.Fatal("want error on nil cert")
	}
}

func TestSPKIToIdentityID_RejectsEmpty(t *testing.T) {
	_, err := server.SPKIToIdentityID(&x509.Certificate{})
	if err == nil {
		t.Fatal("want error on empty SPKI")
	}
}

func TestVerifyClientCert_RejectsRSA(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "rsa"},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Unix(1<<31-1, 0),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &rsaKey.PublicKey, rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	err = server.VerifyClientCert([][]byte{der})
	if err == nil || !strings.Contains(err.Error(), "Ed25519") {
		t.Fatalf("err = %v, want containing 'Ed25519'", err)
	}
}

func TestVerifyClientCert_RejectsEmptyChain(t *testing.T) {
	if err := server.VerifyClientCert(nil); err == nil {
		t.Fatal("want error on empty chain")
	}
}

func TestVerifyClientCert_RejectsMultipleCerts(t *testing.T) {
	priv := loadKey(t, "client")
	cert, _ := server.ServerCertFromIdentity(priv)
	err := server.VerifyClientCert([][]byte{cert.Certificate[0], cert.Certificate[0]})
	if err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("err = %v", err)
	}
}

func TestVerifyClientCert_AcceptsValidEd25519(t *testing.T) {
	priv := loadKey(t, "client")
	cert, _ := server.ServerCertFromIdentity(priv)
	if err := server.VerifyClientCert([][]byte{cert.Certificate[0]}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMakeServerTLSConfig_Defaults(t *testing.T) {
	priv := loadKey(t, "server")
	cert, _ := server.ServerCertFromIdentity(priv)
	cfg := server.MakeServerTLSConfig(cert)
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Fatalf("min version = %#x", cfg.MinVersion)
	}
	if !cfg.SessionTicketsDisabled {
		t.Fatal("session tickets must be disabled (no 0-RTT)")
	}
	if len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != "tokenbay/1" {
		t.Fatalf("ALPN = %v", cfg.NextProtos)
	}
	if cfg.ClientAuth != tls.RequireAnyClientCert {
		t.Fatalf("ClientAuth = %v", cfg.ClientAuth)
	}
	if cfg.VerifyPeerCertificate == nil {
		t.Fatal("VerifyPeerCertificate not installed")
	}
}

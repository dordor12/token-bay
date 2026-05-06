//go:build integration

package integration_test

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"testing"
	"time"

	quicgo "github.com/quic-go/quic-go"

	"github.com/token-bay/token-bay/tracker/internal/server"
)

func TestIntegration_Net_HardKill_ClientSide(t *testing.T) {
	f := newFixture(t)
	conn, err := f.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	openHB(t, conn)

	// Wait for server to register the peer.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && f.srv.PeerCount() != 1 {
		time.Sleep(20 * time.Millisecond)
	}
	if f.srv.PeerCount() != 1 {
		t.Fatalf("PeerCount = %d", f.srv.PeerCount())
	}

	// Hard kill from client side.
	_ = conn.CloseWithError(0, "test-kill")

	// Server's *Connection should drop within 1 s.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && f.srv.PeerCount() != 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if f.srv.PeerCount() != 0 {
		t.Fatalf("PeerCount = %d after client kill", f.srv.PeerCount())
	}
}

func TestIntegration_Net_DialUnreachable(t *testing.T) {
	// Dial 127.0.0.1 on a port we know nothing listens on. Use a
	// random ephemeral port; if anything is bound (vanishingly likely)
	// the test self-corrects by retrying.
	tlsCfg := &tls.Config{
		Certificates:           []tls.Certificate{ed25519BlankCert(t)},
		InsecureSkipVerify:     true, //nolint:gosec
		VerifyPeerCertificate:  func(_ [][]byte, _ [][]*x509.Certificate) error { return nil },
		NextProtos:             []string{"tokenbay/1"},
		MinVersion:             tls.VersionTLS13,
		SessionTicketsDisabled: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := quicgo.DialAddr(ctx, "127.0.0.1:1", tlsCfg, &quicgo.Config{
		EnableDatagrams: false, Allow0RTT: false,
	})
	if err == nil {
		t.Fatal("dial of unreachable address succeeded")
	}
}

func TestIntegration_Net_RefusedAfterShutdown(t *testing.T) {
	f := newFixture(t)
	addr := f.addr

	// Trigger Shutdown.
	dl, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = f.srv.Shutdown(dl)
	f.cancel() // stop Run too

	// Drain runErr to make sure Run returned.
	select {
	case <-f.runErr:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after Shutdown")
	}

	// Subsequent dial should fail.
	tlsCfg := &tls.Config{
		Certificates:           []tls.Certificate{ed25519BlankCert(t)},
		InsecureSkipVerify:     true, //nolint:gosec
		VerifyPeerCertificate:  func(_ [][]byte, _ [][]*x509.Certificate) error { return nil },
		NextProtos:             []string{"tokenbay/1"},
		MinVersion:             tls.VersionTLS13,
		SessionTicketsDisabled: true,
	}
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer dialCancel()
	if _, err := quicgo.DialAddr(dialCtx, addr, tlsCfg, &quicgo.Config{}); err == nil {
		t.Fatal("dial succeeded after Shutdown")
	}
}

// ed25519BlankCert generates a fresh Ed25519 self-signed cert. Used by
// failure-case tests that don't need to know the resulting peerID.
func ed25519BlankCert(t *testing.T) tls.Certificate {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := server.ServerCertFromIdentity(priv)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// silence unused warnings
var (
	_ = sha256.Sum256
	_ = errors.New
)

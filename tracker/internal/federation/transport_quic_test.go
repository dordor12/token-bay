package federation_test

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/token-bay/token-bay/tracker/internal/federation"
)

// quicPeer is a small harness combining a key, cert, and SPKI hash.
type quicPeer struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	spki [32]byte
}

func newQUICPeer(t *testing.T) quicPeer {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	cert, _ := federation.CertFromIdentity(priv)
	parsed, _ := x509.ParseCertificate(cert.Certificate[0])
	spki, _ := federation.SPKIHashOfCert(parsed)
	return quicPeer{pub: pub, priv: priv, spki: spki}
}

func openLoopbackQUIC(t *testing.T, p quicPeer) *federation.QUICTransport {
	t.Helper()
	cert, _ := federation.CertFromIdentity(p.priv)
	tr, err := federation.NewQUICTransport(federation.QUICConfig{
		ListenAddr:  "127.0.0.1:0",
		IdleTimeout: 5 * time.Second,
		Cert:        cert,
		HandshakeTO: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewQUICTransport: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

func TestQUICTransport_DialAccept_RoundTrip(t *testing.T) {
	t.Parallel()
	srv := newQUICPeer(t)
	cli := newQUICPeer(t)

	srvT := openLoopbackQUIC(t, srv)
	cliT := openLoopbackQUIC(t, cli)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	accepted := make(chan federation.PeerConn, 1)
	var listenWG sync.WaitGroup
	listenWG.Add(1)
	go func() {
		defer listenWG.Done()
		_ = srvT.Listen(ctx, func(c federation.PeerConn) { accepted <- c })
	}()

	srvAddr := srvT.ListenAddr()
	if srvAddr == "" {
		t.Fatal("server ListenAddr empty")
	}

	conn, err := cliT.Dial(ctx, srvAddr, srv.pub)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	srvConn := <-accepted
	defer srvConn.Close()

	if err := conn.Send(ctx, []byte("hello")); err != nil {
		t.Fatalf("send: %v", err)
	}
	got, err := srvConn.Recv(ctx)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q want hello", got)
	}

	// Server-side RemotePub should be the client's pubkey (raw 32-byte
	// Ed25519). cli.spki is the SPKI hash (sha256 of the DER-encoded
	// SubjectPublicKeyInfo, not the raw pubkey) — keep both around so we
	// also exercise the SPKI-of-pub helper path consumed by Dial.
	if string(srvConn.RemotePub()) != string(cli.pub) {
		t.Fatal("server-side RemotePub does not match client pubkey")
	}
	_ = sha256.Sum256 // import retained for symmetry with cli.spki construction
}

func TestQUICTransport_Dial_RejectsWrongPin(t *testing.T) {
	t.Parallel()
	srv := newQUICPeer(t)
	cli := newQUICPeer(t)
	srvT := openLoopbackQUIC(t, srv)
	cliT := openLoopbackQUIC(t, cli)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		_ = srvT.Listen(ctx, func(c federation.PeerConn) { _ = c.Close() })
	}()

	wrongPub, _, _ := ed25519.GenerateKey(crand.Reader)
	_, err := cliT.Dial(ctx, srvT.ListenAddr(), wrongPub)
	if err == nil {
		t.Fatal("expected dial to fail under wrong pin")
	}
}

func TestQUICTransport_Send_FrameTooLarge(t *testing.T) {
	t.Parallel()
	srv := newQUICPeer(t)
	cli := newQUICPeer(t)
	srvT := openLoopbackQUIC(t, srv)
	cliT := openLoopbackQUIC(t, cli)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = srvT.Listen(ctx, func(c federation.PeerConn) { _ = c.Close() }) }()

	conn, err := cliT.Dial(ctx, srvT.ListenAddr(), srv.pub)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	big := make([]byte, federation.MaxFrameBytes+1)
	if err := conn.Send(ctx, big); !errors.Is(err, federation.ErrFrameTooLarge) {
		t.Fatalf("expected ErrFrameTooLarge, got %v", err)
	}
}

func TestQUICTransport_NewQUICTransport_BindFailureSurfaces(t *testing.T) {
	t.Parallel()
	_, priv, _ := ed25519.GenerateKey(crand.Reader)
	cert, _ := federation.CertFromIdentity(priv)

	tr1, err := federation.NewQUICTransport(federation.QUICConfig{
		ListenAddr: "127.0.0.1:0", IdleTimeout: time.Second, Cert: cert, HandshakeTO: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	addr := tr1.ListenAddr()
	defer tr1.Close()

	_, err = federation.NewQUICTransport(federation.QUICConfig{
		ListenAddr: addr, IdleTimeout: time.Second, Cert: cert, HandshakeTO: time.Second,
	})
	if err == nil {
		t.Fatalf("expected bind error on %s", addr)
	}
}

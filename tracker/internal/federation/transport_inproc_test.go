package federation_test

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/token-bay/token-bay/tracker/internal/federation"
)

// b is a shared helper for all federation tests in this package: returns
// an n-byte slice filled with `fill`. Defined here (the first tracker-
// side test file) so subsequent test files can reuse it without a
// dedicated helpers_test.go.
//
//nolint:unused // intentionally kept for subsequent federation test files
func b(n int, fill byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = fill
	}
	return out
}

func TestInprocTransport_DialAccept_RoundTrip(t *testing.T) {
	t.Parallel()
	pub, priv, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hub := federation.NewInprocHub()
	server := federation.NewInprocTransport(hub, "srv", pub, priv)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	accepted := make(chan federation.PeerConn, 1)
	go func() {
		_ = server.Listen(ctx, func(c federation.PeerConn) { accepted <- c })
	}()

	cliPub, cliPriv, _ := ed25519.GenerateKey(crand.Reader)
	client := federation.NewInprocTransport(hub, "cli", cliPub, cliPriv)
	defer client.Close()

	conn, err := client.Dial(ctx, "srv", pub)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	srv := <-accepted

	if err := conn.Send(ctx, []byte("hello")); err != nil {
		t.Fatalf("send: %v", err)
	}
	got, err := srv.Recv(ctx)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q want hello", got)
	}
	if !ed25519.PublicKey(srv.RemotePub()).Equal(cliPub) {
		t.Fatal("remote pub mismatch on server side")
	}
}

func TestInprocTransport_Dial_ExpectedPubMismatch(t *testing.T) {
	t.Parallel()
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	hub := federation.NewInprocHub()
	server := federation.NewInprocTransport(hub, "srv", pub, priv)
	defer server.Close()
	go func() { _ = server.Listen(context.Background(), func(federation.PeerConn) {}) }()

	cliPub, cliPriv, _ := ed25519.GenerateKey(crand.Reader)
	client := federation.NewInprocTransport(hub, "cli", cliPub, cliPriv)
	defer client.Close()

	wrong, _, _ := ed25519.GenerateKey(crand.Reader)
	_, err := client.Dial(context.Background(), "srv", wrong)
	if !errors.Is(err, federation.ErrHandshakeFailed) {
		t.Fatalf("expected ErrHandshakeFailed, got %v", err)
	}
}

func TestInprocTransport_Send_FrameTooLarge(t *testing.T) {
	t.Parallel()
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	hub := federation.NewInprocHub()
	server := federation.NewInprocTransport(hub, "srv", pub, priv)
	defer server.Close()
	go func() { _ = server.Listen(context.Background(), func(federation.PeerConn) {}) }()

	cliPub, cliPriv, _ := ed25519.GenerateKey(crand.Reader)
	client := federation.NewInprocTransport(hub, "cli", cliPub, cliPriv)
	defer client.Close()
	conn, _ := client.Dial(context.Background(), "srv", pub)

	big := make([]byte, federation.MaxFrameBytes+1)
	if err := conn.Send(context.Background(), big); !errors.Is(err, federation.ErrFrameTooLarge) {
		t.Fatalf("expected ErrFrameTooLarge, got %v", err)
	}
}

func TestInprocTransport_Send_AfterClose_ReturnsErrPeerClosed(t *testing.T) {
	t.Parallel()
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	hub := federation.NewInprocHub()
	server := federation.NewInprocTransport(hub, "srv", pub, priv)
	defer server.Close()
	go func() { _ = server.Listen(context.Background(), func(federation.PeerConn) {}) }()

	cliPub, cliPriv, _ := ed25519.GenerateKey(crand.Reader)
	client := federation.NewInprocTransport(hub, "cli", cliPub, cliPriv)
	defer client.Close()

	conn, err := client.Dial(context.Background(), "srv", pub)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Must NOT panic; must return ErrPeerClosed.
	if err := conn.Send(context.Background(), []byte("late")); !errors.Is(err, federation.ErrPeerClosed) {
		t.Fatalf("Send after Close: got %v, want ErrPeerClosed", err)
	}
}

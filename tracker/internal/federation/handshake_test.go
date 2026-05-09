package federation_test

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/federation"
)

type peerCfg struct {
	id   ids.TrackerID
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newPeerCfg(t *testing.T) peerCfg {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	return peerCfg{id: ids.TrackerID(sha256.Sum256(pub)), pub: pub, priv: priv}
}

func TestHandshake_DialerInitiator_Success(t *testing.T) {
	t.Parallel()
	srv := newPeerCfg(t)
	cli := newPeerCfg(t)

	hub := federation.NewInprocHub()
	srvT := federation.NewInprocTransport(hub, "srv", srv.pub, srv.priv)
	defer srvT.Close()
	cliT := federation.NewInprocTransport(hub, "cli", cli.pub, cli.priv)
	defer cliT.Close()

	expected := map[ids.TrackerID]ed25519.PublicKey{
		srv.id: srv.pub, cli.id: cli.pub,
	}
	srvDone := make(chan error, 1)
	go func() {
		_ = srvT.Listen(context.Background(), func(c federation.PeerConn) {
			_, err := federation.RunHandshakeListener(context.Background(), c, srv.id, srv.priv, expected, time.Second)
			srvDone <- err
		})
	}()

	conn, err := cliT.Dial(context.Background(), "srv", srv.pub)
	if err != nil {
		t.Fatal(err)
	}
	_, err = federation.RunHandshakeDialer(context.Background(), conn, cli.id, cli.priv, srv.id, srv.pub, time.Second)
	if err != nil {
		t.Fatalf("dialer handshake: %v", err)
	}
	if err := <-srvDone; err != nil {
		t.Fatalf("listener handshake: %v", err)
	}
}

func TestHandshake_RejectsUnknownPeer(t *testing.T) {
	t.Parallel()
	srv := newPeerCfg(t)
	cli := newPeerCfg(t)

	hub := federation.NewInprocHub()
	srvT := federation.NewInprocTransport(hub, "srv", srv.pub, srv.priv)
	defer srvT.Close()
	cliT := federation.NewInprocTransport(hub, "cli", cli.pub, cli.priv)
	defer cliT.Close()

	expected := map[ids.TrackerID]ed25519.PublicKey{srv.id: srv.pub} // cli not in allowlist
	srvDone := make(chan error, 1)
	go func() {
		_ = srvT.Listen(context.Background(), func(c federation.PeerConn) {
			_, err := federation.RunHandshakeListener(context.Background(), c, srv.id, srv.priv, expected, time.Second)
			srvDone <- err
		})
	}()
	conn, _ := cliT.Dial(context.Background(), "srv", srv.pub)
	_, err := federation.RunHandshakeDialer(context.Background(), conn, cli.id, cli.priv, srv.id, srv.pub, time.Second)
	if !errors.Is(err, federation.ErrHandshakeFailed) {
		t.Fatalf("expected ErrHandshakeFailed, got %v", err)
	}
	if err := <-srvDone; !errors.Is(err, federation.ErrHandshakeFailed) {
		t.Fatalf("listener: expected ErrHandshakeFailed, got %v", err)
	}
}

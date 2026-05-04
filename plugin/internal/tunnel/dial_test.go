package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loopbackListener spins up a bare quic-go listener with the seeder
// config; the test interacts with it directly via quic-go to exercise
// Dial without needing the full Listen API (Task 8).
func loopbackListener(t *testing.T, seederPriv ed25519.PrivateKey, consumerPub ed25519.PublicKey) (netip.AddrPort, *quic.Listener, func()) {
	t.Helper()
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)

	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	tlsCfg, err := newTLSConfig(seederPriv, consumerPub, now, true)
	require.NoError(t, err)

	tr := &quic.Transport{Conn: udp}
	ln, err := tr.Listen(tlsCfg, &quic.Config{
		HandshakeIdleTimeout: 5 * time.Second,
		MaxIdleTimeout:       30 * time.Second,
	})
	require.NoError(t, err)

	addrPort := udp.LocalAddr().(*net.UDPAddr).AddrPort()
	cleanup := func() {
		_ = ln.Close()
		_ = tr.Close()
	}
	return addrPort, ln, cleanup
}

func TestDial_HandshakesAndOpensStream(t *testing.T) {
	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	addr, ln, cleanup := loopbackListener(t, seederPriv, consumerPub)
	defer cleanup()

	// Seeder side: accept the connection, then accept the stream (quic-go
	// only surfaces a peer-opened stream after the peer sends bytes on it,
	// so we wait on AcceptStream until the consumer writes its first frame).
	seederDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := ln.Accept(ctx)
		if err != nil {
			seederDone <- err
			return
		}
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			seederDone <- err
			return
		}
		_ = stream.Close()
		_ = conn.CloseWithError(0, "ok")
		seederDone <- nil
	}()

	// Consumer side.
	cfg := Config{
		EphemeralPriv: consumerPriv,
		PeerPin:       seederPub,
		Now:           func() time.Time { return time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tun, err := Dial(ctx, addr, cfg)
	require.NoError(t, err)
	require.NotNil(t, tun)
	defer tun.Close()

	// Send a byte so the seeder's AcceptStream can return — quic-go does
	// not signal a fresh stream until the opener writes (or closes) it.
	require.NoError(t, tun.Send([]byte("ping")))

	require.NoError(t, <-seederDone)
}

func TestDial_PinMismatch(t *testing.T) {
	_, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	wrongSeederPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	addr, ln, cleanup := loopbackListener(t, seederPriv, consumerPub)
	defer cleanup()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = ln.Accept(ctx) // ignore — handshake will fail on the consumer side
	}()

	cfg := Config{
		EphemeralPriv: consumerPriv,
		PeerPin:       wrongSeederPub,
		Now:           func() time.Time { return time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err = Dial(ctx, addr, cfg)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrPeerPinMismatch) || errors.Is(err, ErrHandshakeFailed),
		"got %v", err)
}

func TestDial_ContextCancel(t *testing.T) {
	consumerPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_ = consumerPub

	// Address that nothing listens on.
	addr := netip.MustParseAddrPort("127.0.0.1:1") // privileged port no one binds in tests

	cfg := Config{
		EphemeralPriv: consumerPriv,
		PeerPin:       consumerPub, // any 32-byte pub — handshake never starts
		Now:           func() time.Time { return time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err = Dial(ctx, addr, cfg)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrHandshakeFailed) || errors.Is(err, context.DeadlineExceeded),
		"got %v", err)
}

func TestDial_BadConfig(t *testing.T) {
	_, err := Dial(context.Background(), netip.MustParseAddrPort("127.0.0.1:1"), Config{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig))
}

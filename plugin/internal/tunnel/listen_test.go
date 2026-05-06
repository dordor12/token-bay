package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListen_BadConfig(t *testing.T) {
	_, err := Listen(netip.MustParseAddrPort("127.0.0.1:0"), Config{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig))
}

func TestListen_AcceptHandshake(t *testing.T) {
	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	clk := func() time.Time { return time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC) }
	seederCfg := Config{
		EphemeralPriv: seederPriv,
		PeerPin:       consumerPub,
		Now:           clk,
	}
	consumerCfg := Config{
		EphemeralPriv: consumerPriv,
		PeerPin:       seederPub,
		Now:           clk,
	}

	ln, err := Listen(netip.MustParseAddrPort("127.0.0.1:0"), seederCfg)
	require.NoError(t, err)
	defer ln.Close()

	addr := ln.LocalAddr()
	require.True(t, addr.IsValid())
	require.NotEqual(t, uint16(0), addr.Port())

	type acceptResult struct {
		tun *Tunnel
		err error
	}
	accCh := make(chan acceptResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tun, err := ln.Accept(ctx)
		accCh <- acceptResult{tun, err}
	}()

	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	consumerTun, err := Dial(dialCtx, addr, consumerCfg)
	require.NoError(t, err)
	defer consumerTun.Close()

	// Send a byte so the seeder's AcceptStream can return — quic-go does
	// not signal a fresh stream until the opener writes (or closes) it.
	require.NoError(t, consumerTun.Send([]byte("ping")))

	res := <-accCh
	require.NoError(t, res.err)
	require.NotNil(t, res.tun)
	defer res.tun.Close()
}

func TestListen_AcceptCtxCancel(t *testing.T) {
	_, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	cfg := Config{
		EphemeralPriv: seederPriv,
		PeerPin:       consumerPub,
		Now:           func() time.Time { return time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC) },
	}
	ln, err := Listen(netip.MustParseAddrPort("127.0.0.1:0"), cfg)
	require.NoError(t, err)
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = ln.Accept(ctx)
	require.Error(t, err)
	// Either ctx.DeadlineExceeded surfaces directly or it's wrapped as a tunnel error.
	assert.True(t, errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrHandshakeFailed),
		"got %v", err)
}

func TestListener_LocalAddr_AfterClose(t *testing.T) {
	_, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	cfg := Config{
		EphemeralPriv: seederPriv,
		PeerPin:       consumerPub,
		Now:           func() time.Time { return time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC) },
	}
	ln, err := Listen(netip.MustParseAddrPort("127.0.0.1:0"), cfg)
	require.NoError(t, err)
	preCloseAddr := ln.LocalAddr()
	require.NoError(t, ln.Close())

	// Second Close is a no-op (idempotent).
	assert.NoError(t, ln.Close())

	// LocalAddr after close returns the last known address.
	assert.Equal(t, preCloseAddr, ln.LocalAddr())

	// Bad bind also surfaces ErrInvalidConfig.
	_, err = Listen(netip.AddrPort{}, cfg)
	require.Error(t, err)
}

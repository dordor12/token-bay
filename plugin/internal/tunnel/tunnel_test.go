package tunnel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func dialAcceptPair(t *testing.T) (consumer, seeder *Tunnel, cleanup func()) {
	t.Helper()
	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	clk := func() time.Time { return time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC) }

	ln, err := Listen(netip.MustParseAddrPort("127.0.0.1:0"), Config{
		EphemeralPriv: seederPriv, PeerPin: consumerPub, Now: clk,
	})
	require.NoError(t, err)

	type res struct {
		tun *Tunnel
		err error
	}
	ch := make(chan res, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tun, err := ln.Accept(ctx)
		ch <- res{tun, err}
	}()

	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	consumer, err = Dial(dialCtx, ln.LocalAddr(), Config{
		EphemeralPriv: consumerPriv, PeerPin: seederPub, Now: clk,
	})
	require.NoError(t, err)

	// quic-go does not surface a peer-opened stream to AcceptStream
	// until the opener writes (or closes) it. Flush a single byte
	// directly to the underlying stream so Accept can return; we do
	// this below the framing layer so the byte is consumed by the
	// helper and the seeder Tunnel starts at a clean wire boundary.
	// The byte value is arbitrary — drained on the seeder side below.
	_, err = consumer.stream.Write([]byte{0xff})
	require.NoError(t, err)

	r := <-ch
	require.NoError(t, r.err)
	seeder = r.tun

	// Drain the flush byte from the seeder stream so the next read
	// (e.g. ReadRequest) starts at a clean wire boundary.
	var flush [1]byte
	_, err = io.ReadFull(seeder.stream, flush[:])
	require.NoError(t, err)

	return consumer, seeder, func() {
		_ = consumer.Close()
		_ = seeder.Close()
		_ = ln.Close()
	}
}

func TestTunnel_RequestResponse_OK(t *testing.T) {
	consumer, seeder, cleanup := dialAcceptPair(t)
	defer cleanup()

	go func() {
		body, err := seeder.ReadRequest()
		require.NoError(t, err)
		assert.Equal(t, []byte(`{"model":"opus"}`), body)
		require.NoError(t, seeder.SendOK())
		_, err = seeder.ResponseWriter().Write([]byte("event: message_start\n"))
		require.NoError(t, err)
		_, err = seeder.ResponseWriter().Write([]byte("data: {}\n\n"))
		require.NoError(t, err)
		require.NoError(t, seeder.CloseWrite())
	}()

	require.NoError(t, consumer.Send([]byte(`{"model":"opus"}`)))
	st, r, err := consumer.Receive(context.Background())
	require.NoError(t, err)
	assert.Equal(t, StatusOK, st)

	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, []byte("event: message_start\ndata: {}\n\n"), got)
}

func TestTunnel_RequestResponse_PeerError(t *testing.T) {
	consumer, seeder, cleanup := dialAcceptPair(t)
	defer cleanup()

	go func() {
		_, err := seeder.ReadRequest()
		require.NoError(t, err)
		require.NoError(t, seeder.SendError("rate-limited"))
		require.NoError(t, seeder.CloseWrite())
	}()

	require.NoError(t, consumer.Send([]byte(`{}`)))
	st, r, err := consumer.Receive(context.Background())
	require.NoError(t, err)
	assert.Equal(t, StatusError, st)
	body, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, []byte("rate-limited"), body)
}

func TestTunnel_RequestTooLarge(t *testing.T) {
	consumer, seeder, cleanup := dialAcceptPair(t)
	defer cleanup()
	seeder.cfg.MaxRequestBytes = 16
	consumer.cfg.MaxRequestBytes = 16

	go func() {
		_, err := seeder.ReadRequest()
		// We expect ErrRequestTooLarge.
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrRequestTooLarge), "got %v", err)
	}()

	body := bytes.Repeat([]byte("x"), 17)
	require.NoError(t, consumer.Send(body))
	time.Sleep(100 * time.Millisecond) // let the seeder goroutine record err
}

func TestTunnel_Closed_Send(t *testing.T) {
	consumer, seeder, cleanup := dialAcceptPair(t)
	defer cleanup()
	require.NoError(t, consumer.Close())
	err := consumer.Send([]byte("x"))
	assert.True(t, errors.Is(err, ErrTunnelClosed))
	_ = seeder
}

func TestTunnel_Closed_Receive(t *testing.T) {
	consumer, seeder, cleanup := dialAcceptPair(t)
	defer cleanup()
	require.NoError(t, consumer.Close())
	_, _, err := consumer.Receive(context.Background())
	assert.True(t, errors.Is(err, ErrTunnelClosed))
	_ = seeder
}

func TestTunnel_HalfOpenReadEOF(t *testing.T) {
	consumer, seeder, cleanup := dialAcceptPair(t)
	defer cleanup()

	go func() {
		// Seeder reads but never writes a status byte; it just closes
		// its write side immediately.
		_, _ = seeder.ReadRequest()
		_ = seeder.CloseWrite()
	}()

	require.NoError(t, consumer.Send([]byte(`{}`)))
	_, _, err := consumer.Receive(context.Background())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrFramingViolation), "got %v", err)
}

// Closed-state coverage for the seeder-side helpers — same shape as
// TestTunnel_Closed_Send/Receive. Each helper must short-circuit with
// ErrTunnelClosed once Close has landed.
func TestTunnel_Closed_SeederHelpers(t *testing.T) {
	consumer, seeder, cleanup := dialAcceptPair(t)
	defer cleanup()
	require.NoError(t, seeder.Close())

	_, err := seeder.ReadRequest()
	assert.True(t, errors.Is(err, ErrTunnelClosed), "ReadRequest: %v", err)
	assert.True(t, errors.Is(seeder.SendOK(), ErrTunnelClosed))
	assert.True(t, errors.Is(seeder.SendError("x"), ErrTunnelClosed))
	assert.True(t, errors.Is(seeder.CloseWrite(), ErrTunnelClosed))
	_ = consumer
}

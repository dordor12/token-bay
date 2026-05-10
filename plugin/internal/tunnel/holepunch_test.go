package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubRendezvous is a fakable Rendezvous for tests. It records calls and
// can be configured to fail either method.
type stubRendezvous struct {
	reflexive  netip.AddrPort
	allocErr   error
	relay      RelayCoords
	openErr    error
	allocCalls atomic.Int64
	openCalls  atomic.Int64
}

func (s *stubRendezvous) AllocateReflexive(_ context.Context) (netip.AddrPort, error) {
	s.allocCalls.Add(1)
	return s.reflexive, s.allocErr
}

func (s *stubRendezvous) OpenRelay(_ context.Context, _ [16]byte) (RelayCoords, error) {
	s.openCalls.Add(1)
	return s.relay, s.openErr
}

// holepunchListener is a minimal seeder-side QUIC listener using an explicit
// quic.Transport, returning the bound AddrPort. Used by tests that exercise
// rendezvous Dial without invoking Listen's rendezvous mode.
func holepunchListener(t *testing.T, seederPriv ed25519.PrivateKey, consumerPub ed25519.PublicKey) (netip.AddrPort, *quic.Listener, func()) {
	t.Helper()
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	tlsCfg, err := newTLSConfig(seederPriv, consumerPub, now, true)
	require.NoError(t, err)
	tr := &quic.Transport{Conn: udp}
	ln, err := tr.Listen(tlsCfg, &quic.Config{
		HandshakeIdleTimeout: 5 * time.Second,
		MaxIdleTimeout:       30 * time.Second,
	})
	require.NoError(t, err)
	addr := udp.LocalAddr().(*net.UDPAddr).AddrPort()
	cleanup := func() {
		_ = ln.Close()
		_ = tr.Close()
	}
	return addr, ln, cleanup
}

func TestDial_RendezvousHolePunchSucceeds(t *testing.T) {
	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	seederAddr, ln, cleanup := holepunchListener(t, seederPriv, consumerPub)
	defer cleanup()

	// Drain one connection so accept does not stall the goroutine forever.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := ln.Accept(ctx)
		if err != nil {
			return
		}
		_, _ = conn.AcceptStream(ctx)
		_ = conn.CloseWithError(0, "ok")
	}()

	rdv := &stubRendezvous{
		reflexive: netip.MustParseAddrPort("127.0.0.1:0"),
	}
	cfg := Config{
		EphemeralPriv: consumerPriv,
		PeerPin:       seederPub,
		Rendezvous:    rdv,
		Now:           func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tun, err := Dial(ctx, seederAddr, cfg)
	require.NoError(t, err)
	defer tun.Close()

	assert.EqualValues(t, 1, rdv.allocCalls.Load(), "AllocateReflexive should be called exactly once")
	assert.EqualValues(t, 0, rdv.openCalls.Load(), "OpenRelay must NOT be called on hole-punch success")

	wg.Wait()
}

func TestDial_RendezvousAllocateReflexiveError(t *testing.T) {
	_, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	seederPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	seederAddr, _, cleanup := holepunchListener(t, seederPriv, consumerPub)
	defer cleanup()

	stubErr := errors.New("stun rpc broken")
	rdv := &stubRendezvous{allocErr: stubErr}
	cfg := Config{
		EphemeralPriv: consumerPriv,
		PeerPin:       seederPub,
		Rendezvous:    rdv,
		Now:           func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = Dial(ctx, seederAddr, cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, stubErr)
}

// blackholeUDPAddr returns a 127.0.0.1 UDP port that is bound but never
// reads. QUIC packets sent there get queued in the kernel and never reply,
// guaranteeing the consumer's hole-punch attempt times out.
func blackholeUDPAddr(t *testing.T) (netip.AddrPort, func()) {
	t.Helper()
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)
	addr := udp.LocalAddr().(*net.UDPAddr).AddrPort()
	return addr, func() { _ = udp.Close() }
}

func TestDial_RendezvousFallsBackToRelay(t *testing.T) {
	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	// Real seeder listening on a real address — what the relay "forwards
	// to" in our test (we do not run an actual UDP relay).
	seederAddr, ln, cleanupLn := holepunchListener(t, seederPriv, consumerPub)
	defer cleanupLn()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		conn, err := ln.Accept(ctx)
		if err != nil {
			return
		}
		_, _ = conn.AcceptStream(ctx)
		_ = conn.CloseWithError(0, "ok")
	}()

	// Hole-punch target: a black-hole that swallows packets so the QUIC
	// handshake against it definitely times out.
	blackhole, cleanupBh := blackholeUDPAddr(t)
	defer cleanupBh()

	rdv := &stubRendezvous{
		reflexive: netip.MustParseAddrPort("127.0.0.1:0"),
		relay: RelayCoords{
			Endpoint: seederAddr, // production: real relay; test: real seeder
			Token:    []byte("opaque-token"),
		},
	}

	cfg := Config{
		EphemeralPriv:    consumerPriv,
		PeerPin:          seederPub,
		Rendezvous:       rdv,
		HolePunchTimeout: 250 * time.Millisecond, // tight; we want fast fallback
		Now:              func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tun, err := Dial(ctx, blackhole, cfg)
	require.NoError(t, err)
	defer tun.Close()

	assert.EqualValues(t, 1, rdv.allocCalls.Load(), "AllocateReflexive called once")
	assert.EqualValues(t, 1, rdv.openCalls.Load(), "OpenRelay called exactly once after hole-punch failure")

	wg.Wait()
}

func TestDial_RendezvousRelayPinMismatch(t *testing.T) {
	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	wrongPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_ = seederPub

	seederAddr, ln, cleanupLn := holepunchListener(t, seederPriv, consumerPub)
	defer cleanupLn()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := ln.Accept(ctx)
		if err != nil {
			return
		}
		_ = conn.CloseWithError(0, "ok")
	}()

	blackhole, cleanupBh := blackholeUDPAddr(t)
	defer cleanupBh()

	rdv := &stubRendezvous{
		reflexive: netip.MustParseAddrPort("127.0.0.1:0"),
		relay:     RelayCoords{Endpoint: seederAddr, Token: nil},
	}
	cfg := Config{
		EphemeralPriv:    consumerPriv,
		PeerPin:          wrongPub, // intentionally wrong
		Rendezvous:       rdv,
		HolePunchTimeout: 250 * time.Millisecond,
		Now:              func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = Dial(ctx, blackhole, cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPeerPinMismatch, "TLS pin must be enforced on the relay path")
}

func TestDial_RendezvousOpenRelayError(t *testing.T) {
	seederPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	blackhole, cleanupBh := blackholeUDPAddr(t)
	defer cleanupBh()

	stubErr := errors.New("relay allocation refused")
	rdv := &stubRendezvous{
		reflexive: netip.MustParseAddrPort("127.0.0.1:0"),
		openErr:   stubErr,
	}
	cfg := Config{
		EphemeralPriv:    consumerPriv,
		PeerPin:          seederPub,
		Rendezvous:       rdv,
		HolePunchTimeout: 250 * time.Millisecond,
		Now:              func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = Dial(ctx, blackhole, cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRelayFailed)
	assert.ErrorIs(t, err, stubErr)
}

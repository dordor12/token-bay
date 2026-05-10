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

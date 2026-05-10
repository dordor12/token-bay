package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListen_RendezvousCallsAllocateReflexive(t *testing.T) {
	_, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	wantReflexive := netip.MustParseAddrPort("203.0.113.7:51820")
	rdv := &stubRendezvous{reflexive: wantReflexive}

	ln, err := Listen(netip.MustParseAddrPort("127.0.0.1:0"), Config{
		EphemeralPriv: seederPriv,
		PeerPin:       consumerPub,
		Rendezvous:    rdv,
		Now:           func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) },
	})
	require.NoError(t, err)
	defer ln.Close()

	assert.EqualValues(t, 1, rdv.allocCalls.Load())
	assert.Equal(t, wantReflexive, ln.ReflexiveAddr(), "Listener must expose what AllocateReflexive returned")
}

func TestListen_RendezvousAllocateError(t *testing.T) {
	_, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	stubErr := errors.New("stun rpc broken")
	rdv := &stubRendezvous{allocErr: stubErr}

	_, err = Listen(netip.MustParseAddrPort("127.0.0.1:0"), Config{
		EphemeralPriv: seederPriv,
		PeerPin:       consumerPub,
		Rendezvous:    rdv,
		Now:           func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) },
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, stubErr)
}

func TestListen_NoRendezvousSkipsAllocate(t *testing.T) {
	_, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	ln, err := Listen(netip.MustParseAddrPort("127.0.0.1:0"), Config{
		EphemeralPriv: seederPriv,
		PeerPin:       consumerPub,
		// Rendezvous intentionally nil
		Now: func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) },
	})
	require.NoError(t, err)
	defer ln.Close()

	assert.False(t, ln.ReflexiveAddr().IsValid(), "ReflexiveAddr must be zero when rendezvous unused")
}

// TestE2E_RendezvousHolePunchToListenSucceeds boots a Listener in rendezvous
// mode and dials it via Dial in rendezvous mode; both AllocateReflexive
// calls fire and no relay fallback is needed.
func TestE2E_RendezvousHolePunchToListenSucceeds(t *testing.T) {
	clk := func() time.Time { return time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC) }

	seederPub, seederPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	consumerPub, consumerPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	seederRdv := &stubRendezvous{
		reflexive: netip.MustParseAddrPort("198.51.100.1:7"),
	}
	ln, err := Listen(netip.MustParseAddrPort("127.0.0.1:0"), Config{
		EphemeralPriv: seederPriv,
		PeerPin:       consumerPub,
		Rendezvous:    seederRdv,
		Now:           clk,
	})
	require.NoError(t, err)
	defer ln.Close()

	chunks := "event: message_stop\ndata: {}\n\n"
	requestBody := []byte(`{"model":"claude-opus-4-7"}`)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		seederTun, err := ln.Accept(ctx)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		body, err := seederTun.ReadRequest()
		if err != nil {
			t.Errorf("read: %v", err)
			return
		}
		if string(body) != string(requestBody) {
			t.Errorf("body mismatch: got %q want %q", body, requestBody)
			return
		}
		if err := seederTun.SendOK(); err != nil {
			t.Errorf("sendok: %v", err)
			return
		}
		_, _ = seederTun.ResponseWriter().Write([]byte(chunks))
		_ = seederTun.CloseWrite()
	}()

	consumerRdv := &stubRendezvous{
		reflexive: netip.MustParseAddrPort("198.51.100.2:9"),
	}
	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tun, err := Dial(dialCtx, ln.LocalAddr(), Config{
		EphemeralPriv: consumerPriv,
		PeerPin:       seederPub,
		Rendezvous:    consumerRdv,
		Now:           clk,
	})
	require.NoError(t, err)
	defer tun.Close()

	require.NoError(t, tun.Send(requestBody))
	st, r, err := tun.Receive(context.Background())
	require.NoError(t, err)
	assert.Equal(t, StatusOK, st)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, chunks, string(got))

	wg.Wait()

	assert.EqualValues(t, 1, seederRdv.allocCalls.Load())
	assert.EqualValues(t, 1, consumerRdv.allocCalls.Load())
	assert.EqualValues(t, 0, consumerRdv.openCalls.Load(), "no relay fallback expected")
}

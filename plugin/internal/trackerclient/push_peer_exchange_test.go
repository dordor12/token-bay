package trackerclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport/loopback"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient/test/fakeserver"
	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
)

type capturingPeerExchangeHandler struct {
	mu   sync.Mutex
	got  [][]BootstrapPeer
	done chan struct{}
}

func newCapturingHandler() *capturingPeerExchangeHandler {
	return &capturingPeerExchangeHandler{done: make(chan struct{}, 1)}
}

func (c *capturingPeerExchangeHandler) HandlePeerExchange(_ Ctx, peers []BootstrapPeer) error {
	c.mu.Lock()
	c.got = append(c.got, append([]BootstrapPeer(nil), peers...))
	c.mu.Unlock()
	select {
	case c.done <- struct{}{}:
	default:
	}
	return nil
}

func (c *capturingPeerExchangeHandler) snapshot() [][]BootstrapPeer {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]BootstrapPeer, len(c.got))
	copy(out, c.got)
	return out
}

func newWiredClientWithPeerExchange(t *testing.T, h PeerExchangeHandler) (*fakeserver.Server, ed25519.PrivateKey, ids.IdentityID, func()) {
	t.Helper()
	return newWiredClientWithBoth(t, nil, h)
}

func newWiredClientWithBoth(t *testing.T, off OfferHandler, pex PeerExchangeHandler) (*fakeserver.Server, ed25519.PrivateKey, ids.IdentityID, func()) {
	t.Helper()
	cli, srv := loopback.Pair(ids.IdentityID{1}, ids.IdentityID{2})
	drv := loopback.NewDriver()
	drv.Listen("addr:1", srv)

	fake := fakeserver.New(srv)
	cfg := validConfig(t)
	cfg.Transport = drv
	cfg.Endpoints[0].Addr = "addr:1"
	cfg.OfferHandler = off
	cfg.PeerExchangeHandler = pex
	c, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, c.Start(context.Background()))

	serverDone := make(chan struct{})
	go func() {
		_ = fake.Run(context.Background())
		close(serverDone)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, c.WaitConnected(ctx))

	// Generate a tracker keypair so the test can sign a PEER_EXCHANGE
	// envelope with a known key.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	var trackerID ids.IdentityID
	copy(trackerID[:], pub) // good enough for the test — the plugin's
	// dispatcher does not currently re-verify against the connection
	// peer pubkey; the validate-shape gate is the contract.

	_ = cli
	return fake, priv, trackerID, func() {
		_ = c.Close()
		_ = srv.Close()
		<-serverDone
	}
}

func TestPeerExchangeHandler_Merged(t *testing.T) {
	h := newCapturingHandler()
	fake, priv, trackerID, cleanup := newWiredClientWithPeerExchange(t, h)
	defer cleanup()

	// Build a PeerExchange with two entries.
	peers := []*fed.KnownPeer{
		{
			TrackerId:   bytes32(0xAA),
			Addr:        "tracker-a.example.org:8443",
			LastSeen:    1_700_000_000,
			RegionHint:  "eu-central-1",
			HealthScore: 0.9,
		},
		{
			TrackerId:   bytes32(0xBB),
			Addr:        "tracker-b.example.org:8443",
			LastSeen:    1_700_000_000,
			RegionHint:  "us-east-1",
			HealthScore: 0.6,
		},
	}
	pe := &fed.PeerExchange{Peers: peers}
	payload, err := proto.Marshal(pe)
	require.NoError(t, err)

	env := &fed.Envelope{
		SenderId:  trackerID[:],
		Kind:      fed.Kind_KIND_PEER_EXCHANGE,
		Payload:   payload,
		SenderSig: ed25519.Sign(priv, payload),
	}
	require.NoError(t, fake.PushPeerExchange(context.Background(), env))

	// Wait for the handler to be invoked.
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		t.Fatal("PeerExchangeHandler not invoked within deadline")
	}

	got := h.snapshot()
	require.Len(t, got, 1)
	require.Len(t, got[0], 2)
	assert.Equal(t, "tracker-a.example.org:8443", got[0][0].Addr)
	assert.Equal(t, "eu-central-1", got[0][0].RegionHint)
	assert.InDelta(t, 0.9, got[0][0].HealthScore, 0.001)
	assert.Equal(t, byte(0xAA), got[0][0].TrackerID[0])
}

func TestPeerExchangeHandler_NilHandlerDropsSilently(t *testing.T) {
	// PeerExchangeHandler nil — push must not crash and must not invoke
	// anything. A non-nil OfferHandler keeps the push acceptor running
	// so the test exercises the dispatcher's per-tag-nil-handler branch
	// (matching the production "seeder-only deployment" shape).
	h := fakeOfferHandler{accept: false}
	fake, priv, trackerID, cleanup := newWiredClientWithBoth(t, h, nil)
	defer cleanup()

	pe := &fed.PeerExchange{Peers: []*fed.KnownPeer{{
		TrackerId:   bytes32(0xCC),
		Addr:        "x.example.org:8443",
		LastSeen:    1_700_000_000,
		HealthScore: 0.5,
	}}}
	payload, err := proto.Marshal(pe)
	require.NoError(t, err)
	env := &fed.Envelope{
		SenderId:  trackerID[:],
		Kind:      fed.Kind_KIND_PEER_EXCHANGE,
		Payload:   payload,
		SenderSig: ed25519.Sign(priv, payload),
	}
	require.NoError(t, fake.PushPeerExchange(context.Background(), env))
	// Nothing to assert on the receive side; absence of panic + no error
	// from PushPeerExchange is the contract.
}

func TestPeerExchangeHandler_RejectsBadEnvelope(t *testing.T) {
	h := newCapturingHandler()
	fake, _, _, cleanup := newWiredClientWithPeerExchange(t, h)
	defer cleanup()

	// Build a malformed envelope (bad sender_sig length).
	pe := &fed.PeerExchange{Peers: []*fed.KnownPeer{{
		TrackerId:   bytes32(0xDD),
		Addr:        "y.example.org:8443",
		LastSeen:    1_700_000_000,
		HealthScore: 0.5,
	}}}
	payload, err := proto.Marshal(pe)
	require.NoError(t, err)
	env := &fed.Envelope{
		SenderId:  bytes32(0x01),
		Kind:      fed.Kind_KIND_PEER_EXCHANGE,
		Payload:   payload,
		SenderSig: make([]byte, 10), // wrong length — ValidateEnvelope rejects
	}
	require.NoError(t, fake.PushPeerExchange(context.Background(), env))

	select {
	case <-h.done:
		t.Fatal("handler should NOT be invoked for malformed envelope")
	case <-time.After(200 * time.Millisecond):
		// expected — handler not called
	}
	require.Empty(t, h.snapshot())
}

// bytes32 returns a 32-byte slice filled with `fill`.
func bytes32(fill byte) []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = fill
	}
	return b
}

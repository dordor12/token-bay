package trackerclient

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport/loopback"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient/test/fakeserver"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

type fakeOfferHandler struct{ accept bool }

func (f fakeOfferHandler) HandleOffer(_ Ctx, _ *Offer) (OfferDecision, error) {
	if !f.accept {
		return OfferDecision{Accept: false, RejectReason: "no thanks"}, nil
	}
	pk := make([]byte, 32)
	pk[0] = 1
	return OfferDecision{Accept: true, EphemeralPubkey: pk}, nil
}

// recordingOfferHandler captures the *Offer it receives for inspection.
type recordingOfferHandler struct {
	mu    sync.Mutex
	last  *Offer
	reply OfferDecision
}

func (r *recordingOfferHandler) HandleOffer(_ Ctx, o *Offer) (OfferDecision, error) {
	r.mu.Lock()
	r.last = o
	r.mu.Unlock()
	return r.reply, nil
}

func (r *recordingOfferHandler) Last() *Offer {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

func newWiredClientWithOffer(t *testing.T, accept bool) (*Client, *fakeserver.Server, func()) {
	t.Helper()
	cli, srv := loopback.Pair(ids.IdentityID{1}, ids.IdentityID{2})
	drv := loopback.NewDriver()
	drv.Listen("addr:1", srv)

	fake := fakeserver.New(srv)
	cfg := validConfig(t)
	cfg.Transport = drv
	cfg.Endpoints[0].Addr = "addr:1"
	cfg.OfferHandler = fakeOfferHandler{accept: accept}
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
	_ = cli
	return c, fake, func() {
		_ = c.Close()
		_ = srv.Close()
		<-serverDone
	}
}

func TestOfferHandlerAccept(t *testing.T) {
	_, fake, cleanup := newWiredClientWithOffer(t, true)
	defer cleanup()

	push := &tbproto.OfferPush{
		ConsumerId:   make([]byte, 32),
		EnvelopeHash: make([]byte, 32),
		Model:        "claude-sonnet-4-6",
	}
	dec, err := fake.PushOffer(context.Background(), push)
	require.NoError(t, err)
	assert.True(t, dec.Accept)
	assert.Len(t, dec.EphemeralPubkey, 32)
}

func TestOfferHandlerReject(t *testing.T) {
	_, fake, cleanup := newWiredClientWithOffer(t, false)
	defer cleanup()

	push := &tbproto.OfferPush{
		ConsumerId:   make([]byte, 32),
		EnvelopeHash: make([]byte, 32),
		Model:        "claude-sonnet-4-6",
	}
	dec, err := fake.PushOffer(context.Background(), push)
	require.NoError(t, err)
	assert.False(t, dec.Accept)
	assert.Equal(t, "no thanks", dec.RejectReason)
}

func TestOfferHandler_PlumbsConsumerEphemeralPub(t *testing.T) {
	rec := &recordingOfferHandler{reply: OfferDecision{
		Accept:          true,
		EphemeralPubkey: make([]byte, 32),
	}}

	cli, srv := loopback.Pair(ids.IdentityID{1}, ids.IdentityID{2})
	drv := loopback.NewDriver()
	drv.Listen("addr:1", srv)
	fake := fakeserver.New(srv)
	cfg := validConfig(t)
	cfg.Transport = drv
	cfg.Endpoints[0].Addr = "addr:1"
	cfg.OfferHandler = rec
	c, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, c.Start(context.Background()))
	defer c.Close()
	_ = cli

	serverDone := make(chan struct{})
	go func() {
		_ = fake.Run(context.Background())
		close(serverDone)
	}()
	defer func() {
		_ = srv.Close()
		<-serverDone
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, c.WaitConnected(ctx))

	consumerPub := make([]byte, 32)
	for i := range consumerPub {
		consumerPub[i] = byte(0xa0 + i)
	}
	reqID := make([]byte, 16)
	for i := range reqID {
		reqID[i] = byte(0x10 + i)
	}
	push := &tbproto.OfferPush{
		ConsumerId:           make([]byte, 32),
		EnvelopeHash:         make([]byte, 32),
		Model:                "claude-sonnet-4-6",
		ConsumerEphemeralPub: consumerPub,
		RequestId:            reqID,
	}
	dec, err := fake.PushOffer(context.Background(), push)
	require.NoError(t, err)
	assert.True(t, dec.Accept)
	got := rec.Last()
	require.NotNil(t, got)
	assert.Equal(t, consumerPub, got.ConsumerEphemeralPub, "trackerclient must plumb ConsumerEphemeralPub from the push into Offer")
	assert.Equal(t, reqID, got.RequestID[:], "trackerclient must plumb the tracker's request_id (reservation token) from the push into Offer")
}

func TestOfferHandlerInvalidPushRejects(t *testing.T) {
	_, fake, cleanup := newWiredClientWithOffer(t, true)
	defer cleanup()

	bad := &tbproto.OfferPush{
		ConsumerId:   make([]byte, 31), // wrong length
		EnvelopeHash: make([]byte, 32),
		Model:        "x",
	}
	dec, err := fake.PushOffer(context.Background(), bad)
	require.NoError(t, err)
	assert.False(t, dec.Accept)
	assert.Contains(t, dec.RejectReason, "ConsumerId")
}

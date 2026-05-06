package trackerclient

import (
	"context"
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

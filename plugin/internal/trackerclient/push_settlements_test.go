package trackerclient

import (
	"context"
	"errors"
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

type fakeSettlementHandler struct {
	mu       sync.Mutex
	captured []*SettlementRequest
	err      error
}

func (f *fakeSettlementHandler) HandleSettlement(_ Ctx, r *SettlementRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *r
	f.captured = append(f.captured, &cp)
	return f.err
}

func (f *fakeSettlementHandler) snapshot() []*SettlementRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*SettlementRequest, len(f.captured))
	copy(out, f.captured)
	return out
}

func newWiredClientWithSettlement(t *testing.T, h *fakeSettlementHandler) (*Client, *fakeserver.Server, func()) {
	t.Helper()
	cli, srv := loopback.Pair(ids.IdentityID{1}, ids.IdentityID{2})
	drv := loopback.NewDriver()
	drv.Listen("addr:1", srv)

	fake := fakeserver.New(srv)
	cfg := validConfig(t)
	cfg.Transport = drv
	cfg.Endpoints[0].Addr = "addr:1"
	cfg.SettlementHandler = h

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

func TestSettlementHandlerAcksOnSuccessAndCapturesPush(t *testing.T) {
	h := &fakeSettlementHandler{}
	_, fake, cleanup := newWiredClientWithSettlement(t, h)
	defer cleanup()

	wantHash := make([]byte, 32)
	wantHash[0] = 0xAB
	wantHash[31] = 0xCD
	push := &tbproto.SettlementPush{
		PreimageHash: wantHash,
		PreimageBody: []byte("the-preimage-body-bytes"),
	}
	require.NoError(t, fake.PushSettlement(context.Background(), push))

	got := h.snapshot()
	require.Len(t, got, 1)
	assert.Equal(t, [32]byte{0xAB, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xCD}, got[0].PreimageHash)
	assert.Equal(t, []byte("the-preimage-body-bytes"), got[0].PreimageBody)
}

func TestSettlementHandlerErrorSuppressesAck(t *testing.T) {
	h := &fakeSettlementHandler{err: errors.New("over budget")}
	_, fake, cleanup := newWiredClientWithSettlement(t, h)
	defer cleanup()

	push := &tbproto.SettlementPush{
		PreimageHash: make([]byte, 32),
		PreimageBody: []byte("body"),
	}
	requirePushNoAck(t, fake, push)

	require.Len(t, h.snapshot(), 1, "handler must run before refusing")
}

func TestSettlementHandlerNilDropsPushSilently(t *testing.T) {
	cli, srv := loopback.Pair(ids.IdentityID{1}, ids.IdentityID{2})
	drv := loopback.NewDriver()
	drv.Listen("addr:1", srv)

	fake := fakeserver.New(srv)
	cfg := validConfig(t)
	cfg.Transport = drv
	cfg.Endpoints[0].Addr = "addr:1"
	// SettlementHandler intentionally nil: pushes are dropped silently.

	c, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, c.Start(context.Background()))
	defer func() { _ = c.Close() }()

	serverDone := make(chan struct{})
	go func() {
		_ = fake.Run(context.Background())
		close(serverDone)
	}()
	defer func() { _ = srv.Close(); <-serverDone }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, c.WaitConnected(ctx))
	_ = cli

	push := &tbproto.SettlementPush{
		PreimageHash: make([]byte, 32),
		PreimageBody: []byte("body"),
	}
	requirePushNoAck(t, fake, push)
}

// requirePushNoAck pushes a settlement and asserts the call does NOT
// return a SettleAck within a short window — useful for refusal/no-handler
// paths where the ack is intentionally suppressed.
func requirePushNoAck(t *testing.T, fake *fakeserver.Server, push *tbproto.SettlementPush) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fake.PushSettlement(context.Background(), push) }()
	select {
	case err := <-done:
		require.Error(t, err, "expected push to fail (no ack), but it returned cleanly")
	case <-time.After(250 * time.Millisecond):
		// Push is still blocked reading SettleAck — expected when the
		// handler refused or no handler is configured. Acceptable.
	}
}

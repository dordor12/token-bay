package trackerclient

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport/loopback"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient/test/fakeserver"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

type fakeSettlementHandler struct {
	sig []byte
	err error
}

func (f fakeSettlementHandler) HandleSettlement(_ Ctx, _ *SettlementRequest) ([]byte, error) {
	return f.sig, f.err
}

func newWiredClientWithSettlement(t *testing.T, sig []byte) (*Client, *fakeserver.Server, func()) {
	t.Helper()
	cli, srv := loopback.Pair(ids.IdentityID{1}, ids.IdentityID{2})
	drv := loopback.NewDriver()
	drv.Listen("addr:1", srv)

	fake := fakeserver.New(srv)
	cfg := validConfig(t)
	cfg.Transport = drv
	cfg.Endpoints[0].Addr = "addr:1"
	cfg.SettlementHandler = fakeSettlementHandler{sig: sig}

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

func TestSettlementHandlerAck(t *testing.T) {
	sig := make([]byte, 64)
	sig[0] = 0xff
	_, fake, cleanup := newWiredClientWithSettlement(t, sig)
	defer cleanup()

	push := &tbproto.SettlementPush{
		PreimageHash: make([]byte, 32),
		PreimageBody: []byte("body"),
	}
	require.NoError(t, fake.PushSettlement(context.Background(), push))
}

package server_test

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/api"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/server"
)

// stubDispatcher satisfies server.Dispatcher.
type stubDispatcher struct {
	resp *tbproto.RpcResponse
}

func (s *stubDispatcher) Dispatch(_ context.Context, _ *api.RequestCtx, _ *tbproto.RpcRequest) *tbproto.RpcResponse {
	return s.resp
}
func (s *stubDispatcher) PushAPI() api.PushAPI { return api.PushAPI{} }

func validDeps() server.Deps {
	return server.Deps{
		Config:  &config.Config{Server: config.ServerConfig{MaxFrameSize: 1 << 20, IdleTimeoutS: 60, MaxIncomingStreams: 1024, ShutdownGraceS: 30}},
		Logger:  zerolog.Nop(),
		Now:     time.Now,
		Reflect: func(a netip.AddrPort) netip.AddrPort { return a },
		API:     &stubDispatcher{},
	}
}

func TestNew_RequiresConfig(t *testing.T) {
	d := validDeps()
	d.Config = nil
	if _, err := server.New(d); err == nil {
		t.Fatal("want error when Config nil")
	}
}

func TestNew_RequiresAPI(t *testing.T) {
	d := validDeps()
	d.API = nil
	if _, err := server.New(d); err == nil {
		t.Fatal("want error when API nil")
	}
}

func TestNew_OK(t *testing.T) {
	srv, err := server.New(validDeps())
	if err != nil {
		t.Fatal(err)
	}
	if srv == nil {
		t.Fatal("nil server")
	}
}

func TestPeerCount_ZeroBeforeRun(t *testing.T) {
	srv, _ := server.New(validDeps())
	if got := srv.PeerCount(); got != 0 {
		t.Fatalf("PeerCount = %d, want 0", got)
	}
}

func TestPushOfferTo_NotConnected(t *testing.T) {
	srv, _ := server.New(validDeps())
	ch, ok := srv.PushOfferTo(ids.IdentityID{1}, &tbproto.OfferPush{})
	if ok {
		t.Fatal("ok = true on not-running server")
	}
	if ch != nil {
		t.Fatal("non-nil channel on not-running server")
	}
}

func TestPushSettlementTo_NotConnected(t *testing.T) {
	srv, _ := server.New(validDeps())
	ch, ok := srv.PushSettlementTo(ids.IdentityID{1}, &tbproto.SettlementPush{})
	if ok || ch != nil {
		t.Fatalf("got ch=%v, ok=%v", ch, ok)
	}
}

func TestShutdown_BeforeRun_Nil(t *testing.T) {
	srv, _ := server.New(validDeps())
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown before Run: %v", err)
	}
}

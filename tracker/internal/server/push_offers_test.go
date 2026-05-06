package server_test

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/api"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/server"
)

// Push happy-path tests live in the component tier (Phase 11) and the
// integration tier (Phase 12) — they need a real QUIC client that
// accepts server-initiated streams. These unit tests cover the state-
// machine paths PushOfferTo / PushSettlementTo are responsible for.

func newSrvWithKey(t *testing.T) *server.Server {
	t.Helper()
	priv := loadKey(t, "server")
	keyPath := writeKey(t, priv)
	router, _ := api.NewRouter(api.Deps{})
	srv, err := server.New(server.Deps{
		Config: &config.Config{Server: config.ServerConfig{
			ListenAddr:         "127.0.0.1:0",
			IdentityKeyPath:    keyPath,
			MaxFrameSize:       1 << 20,
			IdleTimeoutS:       60,
			MaxIncomingStreams: 1024,
			ShutdownGraceS:     5,
		}},
		Logger: zerolog.Nop(),
		Now:    time.Now,
		API:    router,
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestPushOfferTo_NotRunning_ReturnsNilFalse(t *testing.T) {
	srv := newSrvWithKey(t)
	ch, ok := srv.PushOfferTo(ids.IdentityID{1, 2, 3}, &tbproto.OfferPush{})
	if ok {
		t.Fatal("ok = true on not-running server")
	}
	if ch != nil {
		t.Fatal("non-nil channel on not-running server")
	}
}

func TestPushOfferTo_NoMatchingPeer_ReturnsNilFalse(t *testing.T) {
	srv := newSrvWithKey(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	if waitForListen(srv, 2*time.Second) == "" {
		t.Fatal("listener never bound")
	}

	ch, ok := srv.PushOfferTo(ids.IdentityID{0xff}, &tbproto.OfferPush{})
	if ok {
		t.Fatal("ok = true for unconnected peer")
	}
	if ch != nil {
		t.Fatal("non-nil channel for unconnected peer")
	}
}

func TestPushSettlementTo_NotRunning_ReturnsNilFalse(t *testing.T) {
	srv := newSrvWithKey(t)
	ch, ok := srv.PushSettlementTo(ids.IdentityID{1}, &tbproto.SettlementPush{})
	if ok || ch != nil {
		t.Fatalf("got ch=%v ok=%v", ch, ok)
	}
}

func TestPushSettlementTo_NoMatchingPeer_ReturnsNilFalse(t *testing.T) {
	srv := newSrvWithKey(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	if waitForListen(srv, 2*time.Second) == "" {
		t.Fatal("listener never bound")
	}
	ch, ok := srv.PushSettlementTo(ids.IdentityID{0xff}, &tbproto.SettlementPush{})
	if ok || ch != nil {
		t.Fatalf("got ch=%v ok=%v", ch, ok)
	}
}

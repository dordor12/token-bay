package server_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"net/netip"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/api"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/registry"
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

// TestServer_PeerPubkey verifies that PeerPubkey returns the mTLS-derived
// Ed25519 pubkey for a connected peer and (nil, false) after disconnect.
func TestServer_PeerPubkey(t *testing.T) {
	reg, _ := registry.New(8)
	router, err := api.NewRouter(api.Deps{
		Logger:   zerolog.Nop(),
		Registry: reg,
	})
	if err != nil {
		t.Fatal(err)
	}

	srvPriv := loadKey(t, "server")
	keyPath := writeKey(t, srvPriv)

	srv, err := server.New(server.Deps{
		Config: &config.Config{
			Server: config.ServerConfig{
				ListenAddr:         "127.0.0.1:0",
				IdentityKeyPath:    keyPath,
				MaxFrameSize:       1 << 20,
				IdleTimeoutS:       60,
				MaxIncomingStreams: 1024,
				ShutdownGraceS:     5,
			},
		},
		Logger: zerolog.Nop(),
		Now:    time.Now,
		API:    router,
	})
	if err != nil {
		t.Fatal(err)
	}

	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	go func() { _ = srv.Run(runCtx) }()

	addr := waitForListen(srv, 2*time.Second)
	if addr == "" {
		t.Fatal("listener never bound")
	}

	// Generate a fresh client keypair so we know the expected pubkey.
	cliPub, cliPriv, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cliCert, err := server.CertFromIdentity(cliPriv)
	if err != nil {
		t.Fatal(err)
	}

	// Derive the peerID the server will compute for this client.
	parsed, err := x509.ParseCertificate(cliCert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	spki := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
	var peerID ids.IdentityID
	copy(peerID[:], spki[:])

	// Build the server pin for the client TLS config.
	srvCert, _ := server.CertFromIdentity(srvPriv)
	srvParsed, _ := x509.ParseCertificate(srvCert.Certificate[0])
	srvPin := sha256.Sum256(srvParsed.RawSubjectPublicKeyInfo)

	// Override dialClient to use our known keypair.
	cliConn := dialClientWithCert(t, addr, srvPin, cliCert)

	if !waitForPeers(srv, 1, 2*time.Second) {
		t.Fatalf("PeerCount = %d, want 1", srv.PeerCount())
	}

	got, ok := srv.PeerPubkey(peerID)
	if !ok {
		t.Fatal("PeerPubkey: peer not found after connect")
	}
	if !bytes.Equal(got, cliPub) {
		t.Errorf("PeerPubkey mismatch: got %x, want %x", got, cliPub)
	}

	// Open + close a heartbeat stream so serveConn can proceed, then close the conn.
	hb, hbErr := cliConn.OpenStreamSync(context.Background())
	if hbErr == nil {
		_, _ = hb.Write([]byte{0})
		_ = hb.Close()
	}

	_ = cliConn.CloseWithError(0, "bye")
	if !waitForPeers(srv, 0, 3*time.Second) {
		t.Fatalf("PeerCount = %d, want 0 after disconnect", srv.PeerCount())
	}

	_, ok = srv.PeerPubkey(peerID)
	if ok {
		t.Fatal("PeerPubkey: expected ok=false after disconnect")
	}

	runCancel()
}

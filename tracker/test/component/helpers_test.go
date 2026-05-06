// Package component runs full-pipeline server tests over real QUIC on
// 127.0.0.1 (no UDP off-host, no internet). Each test brings up a fresh
// *server.Server with real ledger + registry + stunturn subsystems and
// dials a raw quic-go client.
package component_test

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	quicgo "github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/api"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
	"github.com/token-bay/token-bay/tracker/internal/registry"
	"github.com/token-bay/token-bay/tracker/internal/server"
	"github.com/token-bay/token-bay/tracker/internal/stunturn"
)

// pushTagOffer / pushTagSettlement match api.PushTagOffer /
// api.PushTagSettlement. Duplicated as bytes to avoid cross-package
// imports in test code.
const (
	pushTagOffer      = byte(0x01)
	pushTagSettlement = byte(0x02)
)

// fixture is the shared bringup for component tests.
type fixture struct {
	srv     *server.Server
	led     *ledger.Ledger
	reg     *registry.Registry
	alloc   *stunturn.Allocator
	addr    string
	pin     [32]byte
	cliPriv ed25519.PrivateKey
	cliPeer ids.IdentityID
	runErr  chan error
	cancel  context.CancelFunc
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	tmp := t.TempDir()

	// Generate a fresh server identity for each fixture so tests don't
	// share state via the SPKI pin.
	_, srvPriv, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	srvKeyPath := filepath.Join(tmp, "tracker.key")
	if err := os.WriteFile(srvKeyPath, srvPriv, 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := storage.Open(context.Background(), filepath.Join(tmp, "ledger.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	led, err := ledger.Open(store, srvPriv)
	if err != nil {
		t.Fatal(err)
	}

	reg, err := registry.New(8)
	if err != nil {
		t.Fatal(err)
	}

	alloc, err := stunturn.NewAllocator(stunturn.AllocatorConfig{
		MaxKbpsPerSeeder: 1024,
		SessionTTL:       30 * time.Second,
		Now:              time.Now,
		Rand:             crand.Reader,
	})
	if err != nil {
		t.Fatal(err)
	}

	type stunAdapter struct {
		alloc *stunturn.Allocator
	}
	type stunService interface {
		ReflectAddr(netip.AddrPort) netip.AddrPort
		Allocate(consumer, seeder ids.IdentityID, requestID [16]byte, now time.Time) (stunturn.Session, error)
	}
	router, err := api.NewRouter(api.Deps{
		Logger:   zerolog.Nop(),
		Now:      time.Now,
		Ledger:   led,
		Registry: reg,
		StunTurn: stAdapter{alloc: alloc, reflect: func(a netip.AddrPort) netip.AddrPort { return a }},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = stunService(stAdapter{}) // type assertion compile-check

	srv, err := server.New(server.Deps{
		Config: &config.Config{
			Server: config.ServerConfig{
				ListenAddr:         "127.0.0.1:0",
				IdentityKeyPath:    srvKeyPath,
				MaxFrameSize:       1 << 20,
				IdleTimeoutS:       60,
				MaxIncomingStreams: 1024,
				ShutdownGraceS:     5,
			},
			Broker: config.BrokerConfig{OfferTimeoutMs: 2_000},
			Settlement: config.SettlementConfig{
				SettlementTimeoutS: 5,
			},
		},
		Logger:   zerolog.Nop(),
		Now:      time.Now,
		Registry: reg,
		Ledger:   led,
		StunTurn: alloc,
		Reflect:  func(a netip.AddrPort) netip.AddrPort { return a },
		API:      router,
	})
	if err != nil {
		t.Fatal(err)
	}

	runCtx, runCancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- srv.Run(runCtx) }()

	if !waitForListen(srv, 2*time.Second) {
		runCancel()
		t.Fatal("listener never bound")
	}

	srvCert, _ := server.ServerCertFromIdentity(srvPriv)
	parsed, _ := x509.ParseCertificate(srvCert.Certificate[0])
	pin := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)

	// Generate the client identity once per fixture so the tests can
	// know the peerID for PushOfferTo etc.
	_, cliPriv, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		runCancel()
		t.Fatal(err)
	}
	cliCert, _ := server.ServerCertFromIdentity(cliPriv)
	cliParsed, _ := x509.ParseCertificate(cliCert.Certificate[0])
	cliPeer, _ := server.SPKIToIdentityID(cliParsed)

	// Pre-register the client identity in the registry so advertise/
	// heartbeat handlers don't fail with ErrUnknownSeeder. Production
	// flow: enroll (admission gate) does the Register; component tests
	// short-circuit to avoid pulling admission in.
	reg.Register(registry.SeederRecord{IdentityID: cliPeer})

	f := &fixture{
		srv:     srv,
		led:     led,
		reg:     reg,
		alloc:   alloc,
		addr:    srv.ListenAddr(),
		pin:     pin,
		cliPriv: cliPriv,
		cliPeer: cliPeer,
		runErr:  runErr,
		cancel:  runCancel,
	}
	t.Cleanup(f.shutdown)
	return f
}

func waitForListen(srv *server.Server, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if srv.ListenAddr() != "" {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func (f *fixture) shutdown() {
	dl, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = f.srv.Shutdown(dl)
	f.cancel()
	select {
	case <-f.runErr:
	case <-time.After(2 * time.Second):
	}
}

// dial brings up a QUIC client connection using f.cliPriv (so the
// server-side peerID equals f.cliPeer).
func (f *fixture) dial(t *testing.T) *quicgo.Conn {
	t.Helper()
	cliCert, err := server.ServerCertFromIdentity(f.cliPriv)
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg := &tls.Config{
		Certificates:       []tls.Certificate{cliCert},
		InsecureSkipVerify: true, //nolint:gosec // pin via VerifyPeerCertificate
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			parsed, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			got := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
			if got != f.pin {
				return errors.New("client: server SPKI mismatch")
			}
			return nil
		},
		NextProtos:             []string{"tokenbay/1"},
		MinVersion:             tls.VersionTLS13,
		SessionTicketsDisabled: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := quicgo.DialAddr(ctx, f.addr, tlsCfg, &quicgo.Config{
		EnableDatagrams: false,
		Allow0RTT:       false,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseWithError(0, "test cleanup") })
	return conn
}

// openHeartbeatStream opens the first client-initiated bidi stream so
// the server's serveConn proceeds past its first AcceptStream. Returns
// the stream so the test can write pings if it wants.
func openHeartbeatStream(t *testing.T, conn *quicgo.Conn) *quicgo.Stream {
	t.Helper()
	hb, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hb.Close() })
	return hb
}

// rpcCall opens a stream, writes the request, reads the response.
func rpcCall(t *testing.T, conn *quicgo.Conn, method tbproto.RpcMethod, payload []byte) *tbproto.RpcResponse {
	t.Helper()
	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	req := &tbproto.RpcRequest{Method: method, Payload: payload}
	if err := server.WriteFrame(stream, req, 1<<20); err != nil {
		t.Fatal(err)
	}
	var resp tbproto.RpcResponse
	if err := server.ReadFrame(stream, &resp, 1<<20); err != nil {
		t.Fatalf("read response (method=%v): %v", method, err)
	}
	return &resp
}

// stAdapter mirrors the cmd/run_cmd stunTurnAdapter so component tests
// can wire StunTurnService without importing main.
type stAdapter struct {
	alloc   *stunturn.Allocator
	reflect func(netip.AddrPort) netip.AddrPort
}

func (a stAdapter) ReflectAddr(remote netip.AddrPort) netip.AddrPort {
	if a.reflect == nil {
		return remote
	}
	return a.reflect(remote)
}

func (a stAdapter) Allocate(consumer, seeder ids.IdentityID, requestID [16]byte, now time.Time) (stunturn.Session, error) {
	return a.alloc.Allocate(consumer, seeder, requestID, now)
}

// mustMarshal helper for test bodies.
func mustMarshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// waitForPeers waits for srv.PeerCount() to equal want.
func waitForPeers(t *testing.T, srv *server.Server, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if srv.PeerCount() == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("PeerCount = %d, want %d", srv.PeerCount(), want)
}

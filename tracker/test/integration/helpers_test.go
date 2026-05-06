//go:build integration

// Package integration runs the build-tagged integration tier for
// tracker/internal/server. Real QUIC, real mTLS, real UDP on
// 127.0.0.1, real subsystems (ledger / registry / stunturn).
//
// Run with:
//
//	make -C tracker test-integration
//	# or
//	go test -race -tags=integration ./test/integration/...
package integration_test

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
	"runtime"
	"testing"
	"time"

	quicgo "github.com/quic-go/quic-go"
	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/api"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
	"github.com/token-bay/token-bay/tracker/internal/registry"
	"github.com/token-bay/token-bay/tracker/internal/server"
	"github.com/token-bay/token-bay/tracker/internal/stunturn"
)

// fixture is the shared bringup for integration tests.
type fixture struct {
	srv     *server.Server
	led     *ledger.Ledger
	reg     *registry.Registry
	addr    string
	pin     [32]byte
	cliPriv ed25519.PrivateKey
	cliPeer ids.IdentityID
	runErr  chan error
	cancel  context.CancelFunc
}

// stAdapter satisfies api.StunTurnService.
type stAdapter struct {
	alloc *stunturn.Allocator
}

func (a stAdapter) ReflectAddr(remote netip.AddrPort) netip.AddrPort { return remote }

func (a stAdapter) Allocate(consumer, seeder ids.IdentityID, requestID [16]byte, now time.Time) (stunturn.Session, error) {
	return a.alloc.Allocate(consumer, seeder, requestID, now)
}

// newFixture brings up a server. Optional func opts mutate config
// fields before New.
func newFixture(t *testing.T, opts ...func(*config.Config)) *fixture {
	t.Helper()
	tmp := t.TempDir()

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

	reg, _ := registry.New(8)

	alloc, _ := stunturn.NewAllocator(stunturn.AllocatorConfig{
		MaxKbpsPerSeeder: 1024,
		SessionTTL:       30 * time.Second,
		Now:              time.Now,
		Rand:             crand.Reader,
	})

	router, _ := api.NewRouter(api.Deps{
		Logger:   zerolog.Nop(),
		Now:      time.Now,
		Ledger:   led,
		Registry: reg,
		StunTurn: stAdapter{alloc: alloc},
	})

	cfg := &config.Config{
		Server: config.ServerConfig{
			ListenAddr:         "127.0.0.1:0",
			IdentityKeyPath:    srvKeyPath,
			MaxFrameSize:       1 << 20,
			IdleTimeoutS:       60,
			MaxIncomingStreams: 1024,
			ShutdownGraceS:     5,
		},
		Broker:     config.BrokerConfig{OfferTimeoutMs: 500},
		Settlement: config.SettlementConfig{SettlementTimeoutS: 5},
	}
	for _, opt := range opts {
		opt(cfg)
	}

	srv, err := server.New(server.Deps{
		Config:   cfg,
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

	srvCert, _ := server.CertFromIdentity(srvPriv)
	parsed, _ := x509.ParseCertificate(srvCert.Certificate[0])
	pin := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)

	_, cliPriv, _ := ed25519.GenerateKey(crand.Reader)
	cliCert, _ := server.CertFromIdentity(cliPriv)
	cliParsed, _ := x509.ParseCertificate(cliCert.Certificate[0])
	cliPeer, _ := server.SPKIToIdentityID(cliParsed)
	reg.Register(registry.SeederRecord{IdentityID: cliPeer})

	f := &fixture{
		srv:     srv,
		led:     led,
		reg:     reg,
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

// newFixtureWithOfferTimeout is a thin convenience wrapper for tests
// that need a tight Broker.OfferTimeoutMs. Matches the spec's pattern
// of "subtest config knob".
func newFixtureWithOfferTimeout(t *testing.T, ms int) *fixture {
	t.Helper()
	return newFixture(t, func(c *config.Config) {
		c.Broker.OfferTimeoutMs = ms
	})
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

// dialOpt customizes the client TLS config.
type dialOpt func(*tls.Config)

func withALPN(alpn string) dialOpt {
	return func(c *tls.Config) { c.NextProtos = []string{alpn} }
}

func withMaxTLS(version uint16) dialOpt {
	return func(c *tls.Config) { c.MaxVersion = version }
}

func withRSAClient(t *testing.T) dialOpt {
	t.Helper()
	rsaCert := makeRSACert(t)
	return func(c *tls.Config) {
		c.Certificates = []tls.Certificate{rsaCert}
	}
}

// dial dials the fixture's server; opts let tests inject failure modes.
// Caller MUST close the returned conn.
func (f *fixture) dial(t *testing.T, opts ...dialOpt) (*quicgo.Conn, error) {
	t.Helper()
	cliCert, err := server.CertFromIdentity(f.cliPriv)
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg := &tls.Config{
		Certificates:       []tls.Certificate{cliCert},
		InsecureSkipVerify: true, //nolint:gosec // see VerifyPeerCertificate
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
	for _, opt := range opts {
		opt(tlsCfg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return quicgo.DialAddr(ctx, f.addr, tlsCfg, &quicgo.Config{
		EnableDatagrams: false,
		Allow0RTT:       false,
	})
}

// goroutineLeakCheck snapshots NumGoroutine and registers a t.Cleanup
// that asserts it doesn't grow more than slack. Tolerant of GC
// stragglers and quic-go's background workers.
func goroutineLeakCheck(t *testing.T, slack int) {
	t.Helper()
	before := runtime.NumGoroutine()
	t.Cleanup(func() {
		// Give async cleanup a moment.
		for i := 0; i < 10; i++ {
			if runtime.NumGoroutine() <= before+slack {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		after := runtime.NumGoroutine()
		if after > before+slack {
			t.Errorf("goroutine count grew: before=%d after=%d slack=%d", before, after, slack)
		}
	})
}

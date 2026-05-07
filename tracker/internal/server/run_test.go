package server_test

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	quicgo "github.com/quic-go/quic-go"
	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/tracker/internal/api"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/registry"
	"github.com/token-bay/token-bay/tracker/internal/server"
)

// writeKey writes priv into a temp file and returns the path.
func writeKey(t *testing.T, priv ed25519.PrivateKey) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "id.key")
	if err := os.WriteFile(path, priv, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// dialClientWithCert runs the client side of the wire protocol using a
// provided TLS certificate. Used when the test needs to know the client's
// pubkey in advance (e.g. TestServer_PeerPubkey).
func dialClientWithCert(t *testing.T, addr string, serverPin [32]byte, cliCert tls.Certificate) *quicgo.Conn {
	t.Helper()
	tlsCfg := &tls.Config{
		Certificates:       []tls.Certificate{cliCert},
		InsecureSkipVerify: true, //nolint:gosec // pin via VerifyPeerCertificate
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			parsed, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			got := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
			if got != serverPin {
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
	conn, err := quicgo.DialAddr(ctx, addr, tlsCfg, &quicgo.Config{
		EnableDatagrams: false,
		Allow0RTT:       false,
	})
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// dialClient runs the client side of the wire protocol just far enough
// to verify Run accepts the connection.
func dialClient(t *testing.T, addr string, serverPin [32]byte) *quicgo.Conn {
	t.Helper()
	_, cliPriv, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cliCert, err := server.CertFromIdentity(cliPriv)
	if err != nil {
		t.Fatal(err)
	}
	return dialClientWithCert(t, addr, serverPin, cliCert)
}

func waitForPeers(srv *server.Server, want int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if srv.PeerCount() == want {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func waitForListen(srv *server.Server, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if a := srv.ListenAddr(); a != "" {
			return a
		}
		time.Sleep(20 * time.Millisecond)
	}
	return ""
}

func TestRun_AcceptsConnection_PeerCountToggles(t *testing.T) {
	reg, _ := registry.New(8)
	router, err := api.NewRouter(api.Deps{
		Logger:   zerolog.Nop(),
		Registry: reg,
	})
	if err != nil {
		t.Fatal(err)
	}

	priv := loadKey(t, "server")
	keyPath := writeKey(t, priv)

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
		Logger:   zerolog.Nop(),
		Now:      time.Now,
		Registry: reg,
		API:      router,
	})
	if err != nil {
		t.Fatal(err)
	}

	runErr := make(chan error, 1)
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	go func() {
		runErr <- srv.Run(runCtx)
	}()

	addr := waitForListen(srv, 2*time.Second)
	if addr == "" {
		t.Fatal("listener never bound")
	}

	serverCert, _ := server.CertFromIdentity(priv)
	parsed, _ := x509.ParseCertificate(serverCert.Certificate[0])
	pin := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)

	cliConn := dialClient(t, addr, pin)

	if !waitForPeers(srv, 1, 2*time.Second) {
		t.Fatalf("PeerCount = %d, want 1", srv.PeerCount())
	}

	// Open a stream so serveConn's first AcceptStream returns and the
	// heartbeat goroutine takes ownership. Closing the stream + conn
	// then unwinds the per-peer state.
	hb, err := cliConn.OpenStreamSync(context.Background())
	if err == nil {
		_, _ = hb.Write([]byte{0}) // poke quic-go to flush stream open
		_ = hb.Close()
	}

	_ = cliConn.CloseWithError(0, "bye")
	if !waitForPeers(srv, 0, 3*time.Second) {
		t.Fatalf("PeerCount = %d, want 0 after disconnect", srv.PeerCount())
	}

	runCancel()
	select {
	case <-runErr:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestRun_TwiceReturnsErrAlreadyRunning(t *testing.T) {
	priv := loadKey(t, "server")
	keyPath := writeKey(t, priv)
	router, _ := api.NewRouter(api.Deps{})
	srv, _ := server.New(server.Deps{
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	if waitForListen(srv, 2*time.Second) == "" {
		t.Fatal("listener never bound")
	}

	if err := srv.Run(ctx); !errors.Is(err, server.ErrAlreadyRunning) {
		t.Fatalf("err = %v, want ErrAlreadyRunning", err)
	}
}

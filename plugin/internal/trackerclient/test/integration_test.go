//go:build integration

// Package test holds integration tests that exercise the trackerclient
// against a real quic-go server bound to a loopback UDP port. These tests
// only run under the `integration` build tag so the regular unit suite
// stays fast and hermetic with respect to UDP sockets.
package test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"testing"
	"time"

	quicgo "github.com/quic-go/quic-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/idtls"
	quicdriver "github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport/quic"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/wire"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

type kp struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	hash [32]byte
}

func newKP(t *testing.T) kp {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	cert, err := idtls.CertFromIdentity(priv)
	require.NoError(t, err)
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	require.NoError(t, err)
	return kp{pub: pub, priv: priv, hash: sha256.Sum256(parsed.RawSubjectPublicKeyInfo)}
}

type fakeSigner struct {
	priv ed25519.PrivateKey
	id   ids.IdentityID
}

func (f fakeSigner) Sign(msg []byte) ([]byte, error) { return ed25519.Sign(f.priv, msg), nil }
func (f fakeSigner) PrivateKey() ed25519.PrivateKey  { return f.priv }
func (f fakeSigner) IdentityID() ids.IdentityID      { return f.id }

// TestQuicHandshakeAndOneRPC stands up a real quic-go listener bound to a
// loopback UDP port, points the production trackerclient (with the real
// QUIC driver) at it, and asserts that:
//   - the QUIC + mTLS handshake completes,
//   - WaitConnected returns nil,
//   - a single Settle RPC round-trips successfully.
func TestQuicHandshakeAndOneRPC(t *testing.T) {
	server := newKP(t)
	client := newKP(t)

	cert, err := idtls.CertFromIdentity(server.priv)
	require.NoError(t, err)
	serverTLS := idtls.MakeServerTLSConfig(cert, nil)

	listener, err := quicgo.ListenAddr("127.0.0.1:0", serverTLS, &quicgo.Config{Allow0RTT: false})
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	addr := listener.Addr().String()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept(context.Background())
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseWithError(0, "") }()
		// Spawn a goroutine per stream so a stream that doesn't write
		// immediately (e.g. the supervisor's heartbeat stream, which
		// only ticks every HeartbeatPeriod) doesn't starve other
		// streams (e.g. the Settle RPC stream we want to round-trip).
		// Each handler tries to decode an RpcRequest and replies OK;
		// HeartbeatPing frames will fail to decode and the goroutine
		// just exits without responding -- harmless for this test.
		for {
			stream, err := conn.AcceptStream(context.Background())
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = stream.Close() }()
				var req tbproto.RpcRequest
				if err := wire.Read(stream, &req, 1<<20); err != nil {
					return
				}
				resp := &tbproto.RpcResponse{Status: tbproto.RpcStatus_RPC_STATUS_OK}
				_ = wire.Write(stream, resp, 1<<20)
			}()
		}
	}()

	cfg := trackerclient.Config{
		Endpoints: []trackerclient.TrackerEndpoint{{
			Addr:         addr,
			IdentityHash: server.hash,
		}},
		Identity:  fakeSigner{priv: client.priv},
		Transport: quicdriver.New(),
	}
	c, err := trackerclient.New(cfg)
	require.NoError(t, err)
	require.NoError(t, c.Start(context.Background()))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, c.WaitConnected(ctx))

	err = c.Settle(ctx, make([]byte, 32), make([]byte, 64))
	require.NoError(t, err)

	// Close the client first; this drops the QUIC connection, which
	// unblocks the server goroutine's AcceptStream call. Then close
	// the listener.
	require.NoError(t, c.Close())
	require.NoError(t, listener.Close())
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not shut down")
	}
}

// TestQuicMismatchedSPKIFails asserts the client refuses to consider the
// connection ready when the server's SPKI does not match the configured
// pin, which manifests as WaitConnected hitting the context deadline
// (the supervisor keeps retrying with backoff).
func TestQuicMismatchedSPKIFails(t *testing.T) {
	server := newKP(t)
	client := newKP(t)

	cert, err := idtls.CertFromIdentity(server.priv)
	require.NoError(t, err)
	serverTLS := idtls.MakeServerTLSConfig(cert, nil)

	listener, err := quicgo.ListenAddr("127.0.0.1:0", serverTLS, &quicgo.Config{Allow0RTT: false})
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()
	addr := listener.Addr().String()

	cfg := trackerclient.Config{
		Endpoints: []trackerclient.TrackerEndpoint{{
			Addr:         addr,
			IdentityHash: [32]byte{0xff}, // wrong pin
		}},
		Identity:  fakeSigner{priv: client.priv},
		Transport: quicdriver.New(),
	}
	c, err := trackerclient.New(cfg)
	require.NoError(t, err)
	require.NoError(t, c.Start(context.Background()))
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	err = c.WaitConnected(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	// The Status() snapshot should carry the identity-mismatch error.
	s := c.Status()
	if s.LastError != nil {
		assert.Contains(t, s.LastError.Error(), "identity")
	}
}

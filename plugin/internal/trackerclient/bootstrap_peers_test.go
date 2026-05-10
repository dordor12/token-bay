package trackerclient

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport/loopback"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient/test/fakeserver"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// newWiredClientWithKeys is like newWiredClient but uses
// loopback.PairWithKeys so the client observes the server's ed25519
// pubkey via Conn.PeerPublicKey(). Returns the client, the server
// fake (so tests can register a BOOTSTRAP_PEERS handler), and a
// cleanup func.
func newWiredClientWithKeys(t *testing.T, serverPub ed25519.PublicKey, serverIdentity ids.IdentityID, register func(*fakeserver.Server)) (*Client, func()) {
	t.Helper()
	cli, srv := loopback.PairWithKeys(ids.IdentityID{1}, serverIdentity, nil, serverPub)
	drv := loopback.NewDriver()
	drv.Listen("addr:1", srv)

	fake := fakeserver.New(srv)
	fake.PayloadType[tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS] = func() proto.Message {
		return &tbproto.BootstrapPeersRequest{}
	}
	if register != nil {
		register(fake)
	}

	cfg := validConfig(t)
	cfg.Transport = drv
	cfg.Endpoints[0].Addr = "addr:1"
	cfg.Endpoints[0].IdentityHash = serverIdentity
	c, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, c.Start(context.Background()))

	serverDone := make(chan struct{})
	go func() {
		_ = fake.Run(context.Background())
		close(serverDone)
	}()

	_ = cli
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, c.WaitConnected(ctx))

	return c, func() {
		_ = c.Close()
		_ = srv.Close()
		<-serverDone
	}
}

// signedBootstrapList builds a list signed by priv with the given
// signed_at / expires_at, suitable for a fake handler to return.
func signedBootstrapList(t *testing.T, priv ed25519.PrivateKey, issuer ids.IdentityID, signedAt, expiresAt time.Time, peers []*tbproto.BootstrapPeer) *tbproto.BootstrapPeerList {
	t.Helper()
	list := &tbproto.BootstrapPeerList{
		IssuerId:  issuer[:],
		SignedAt:  uint64(signedAt.Unix()),
		ExpiresAt: uint64(expiresAt.Unix()),
		Peers:     peers,
	}
	require.NoError(t, tbproto.SignBootstrapPeerList(priv, list))
	return list
}

func newServerKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, ids.IdentityID) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	var id ids.IdentityID
	copy(id[:], pub) // synthetic IdentityID mirrors what the conn observes
	return pub, priv, id
}

func TestFetchBootstrapPeers_OK(t *testing.T) {
	pub, priv, issuer := newServerKey(t)
	now := time.Now()
	peerKey := bytes.Repeat([]byte{0x10}, 32)
	list := signedBootstrapList(t, priv, issuer, now, now.Add(10*time.Minute),
		[]*tbproto.BootstrapPeer{
			{TrackerId: peerKey, Addr: "a.example:443", RegionHint: "r1", HealthScore: 0.9, LastSeen: uint64(now.Add(-time.Hour).Unix())},
		})

	c, cleanup := newWiredClientWithKeys(t, pub, issuer, func(s *fakeserver.Server) {
		s.Handlers[tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS] = func(_ context.Context, _ proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
			return tbproto.RpcStatus_RPC_STATUS_OK, list, nil
		}
	})
	defer cleanup()

	got, err := c.FetchBootstrapPeers(context.Background())
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "a.example:443", got[0].Addr)
}

func TestFetchBootstrapPeers_BadSig(t *testing.T) {
	pub, priv, issuer := newServerKey(t)
	now := time.Now()
	list := signedBootstrapList(t, priv, issuer, now, now.Add(10*time.Minute),
		[]*tbproto.BootstrapPeer{{TrackerId: bytes.Repeat([]byte{0x10}, 32), Addr: "a:1", RegionHint: "r", HealthScore: 0.5, LastSeen: 1}})
	list.Peers[0].Addr = "tampered:1"

	c, cleanup := newWiredClientWithKeys(t, pub, issuer, func(s *fakeserver.Server) {
		s.Handlers[tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS] = func(_ context.Context, _ proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
			return tbproto.RpcStatus_RPC_STATUS_OK, list, nil
		}
	})
	defer cleanup()

	_, err := c.FetchBootstrapPeers(context.Background())
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrBootstrapPeerListBadSig))
}

func TestFetchBootstrapPeers_Expired(t *testing.T) {
	pub, priv, issuer := newServerKey(t)
	expired := time.Now().Add(-2 * time.Minute)
	list := signedBootstrapList(t, priv, issuer, expired.Add(-10*time.Minute), expired, nil)

	c, cleanup := newWiredClientWithKeys(t, pub, issuer, func(s *fakeserver.Server) {
		s.Handlers[tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS] = func(_ context.Context, _ proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
			return tbproto.RpcStatus_RPC_STATUS_OK, list, nil
		}
	})
	defer cleanup()

	_, err := c.FetchBootstrapPeers(context.Background())
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrBootstrapPeerListExpired))
}

func TestFetchBootstrapPeers_SkewTolerance(t *testing.T) {
	pub, priv, issuer := newServerKey(t)
	expires := time.Now().Add(-30 * time.Second)
	list := signedBootstrapList(t, priv, issuer, expires.Add(-10*time.Minute), expires, nil)

	c, cleanup := newWiredClientWithKeys(t, pub, issuer, func(s *fakeserver.Server) {
		s.Handlers[tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS] = func(_ context.Context, _ proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
			return tbproto.RpcStatus_RPC_STATUS_OK, list, nil
		}
	})
	defer cleanup()

	_, err := c.FetchBootstrapPeers(context.Background())
	require.NoError(t, err)
}

func TestFetchBootstrapPeers_IdentityMismatch(t *testing.T) {
	pub, priv, _ := newServerKey(t)
	otherIssuer := ids.IdentityID{0x99}
	connectedID := ids.IdentityID{}
	copy(connectedID[:], pub) // conn says it connected to "pub"
	now := time.Now()
	list := signedBootstrapList(t, priv, otherIssuer, now, now.Add(10*time.Minute), nil)

	c, cleanup := newWiredClientWithKeys(t, pub, connectedID, func(s *fakeserver.Server) {
		s.Handlers[tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS] = func(_ context.Context, _ proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
			return tbproto.RpcStatus_RPC_STATUS_OK, list, nil
		}
	})
	defer cleanup()

	_, err := c.FetchBootstrapPeers(context.Background())
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrBootstrapIssuerMismatch))
}

func TestFetchBootstrapPeers_RpcError(t *testing.T) {
	pub, _, issuer := newServerKey(t)
	c, cleanup := newWiredClientWithKeys(t, pub, issuer, func(s *fakeserver.Server) {
		s.Handlers[tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS] = func(_ context.Context, _ proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
			return tbproto.RpcStatus_RPC_STATUS_INTERNAL, nil, &tbproto.RpcError{Code: "BOOTSTRAP_LIST_STORAGE", Message: "disk on fire"}
		}
	})
	defer cleanup()

	_, err := c.FetchBootstrapPeers(context.Background())
	require.Error(t, err)
}

type fakeBootstrapMetrics struct{ counts map[string]int }

func newFakeBootstrapMetrics() *fakeBootstrapMetrics {
	return &fakeBootstrapMetrics{counts: map[string]int{}}
}
func (m *fakeBootstrapMetrics) IncBootstrapPeersFetched(outcome string) { m.counts[outcome]++ }

func TestFetchBootstrapPeers_MetricsOnEmpty(t *testing.T) {
	pub, priv, issuer := newServerKey(t)
	now := time.Now()
	list := signedBootstrapList(t, priv, issuer, now, now.Add(10*time.Minute), nil)

	met := newFakeBootstrapMetrics()
	c, cleanup := newWiredClientWithKeys(t, pub, issuer, func(s *fakeserver.Server) {
		s.Handlers[tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS] = func(_ context.Context, _ proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError) {
			return tbproto.RpcStatus_RPC_STATUS_OK, list, nil
		}
	})
	defer cleanup()
	c.cfg.Metrics = met // inject after construction since the helper builds Config inline

	_, err := c.FetchBootstrapPeers(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, met.counts["empty"])
}

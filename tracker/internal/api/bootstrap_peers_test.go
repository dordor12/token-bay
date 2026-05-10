package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

type fakeBootstrapService struct {
	rows     []storage.KnownPeer
	listErr  error
	signErr  error
	pub      ed25519.PublicKey
	priv     ed25519.PrivateKey
	issuerID ids.IdentityID
	maxPeers int
	ttl      time.Duration
}

func newFakeBootstrapService(t *testing.T) *fakeBootstrapService {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	var id ids.IdentityID
	copy(id[:], pub) // synthetic — real adapter would hash SPKI
	return &fakeBootstrapService{
		pub: pub, priv: priv, issuerID: id,
		maxPeers: 50, ttl: 10 * time.Minute,
	}
}

func (f *fakeBootstrapService) ListKnownPeers(_ context.Context, limit int, _ bool) ([]storage.KnownPeer, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := f.rows
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeBootstrapService) IssuerID() ids.IdentityID { return f.issuerID }

func (f *fakeBootstrapService) Sign(canonical []byte) ([]byte, error) {
	if f.signErr != nil {
		return nil, f.signErr
	}
	return ed25519.Sign(f.priv, canonical), nil
}

func (f *fakeBootstrapService) MaxPeers() int      { return f.maxPeers }
func (f *fakeBootstrapService) TTL() time.Duration { return f.ttl }

func newRouterWithBootstrap(t *testing.T, svc BootstrapPeersService) *Router {
	t.Helper()
	r, err := NewRouter(Deps{
		Logger:         zerolog.Nop(),
		Now:            func() time.Time { return time.Unix(1714000000, 0) },
		BootstrapPeers: svc,
	})
	require.NoError(t, err)
	return r
}

func TestBootstrapPeers_OK(t *testing.T) {
	svc := newFakeBootstrapService(t)
	svc.rows = []storage.KnownPeer{
		{TrackerID: bytes.Repeat([]byte{0x10}, 32), Addr: "a.example:443", RegionHint: "r1", HealthScore: 0.9, LastSeen: time.Unix(1713999000, 0), Source: "allowlist"},
		{TrackerID: bytes.Repeat([]byte{0x11}, 32), Addr: "b.example:443", RegionHint: "r2", HealthScore: 0.7, LastSeen: time.Unix(1713998000, 0), Source: "gossip"},
	}
	r := newRouterWithBootstrap(t, svc)
	resp := r.Dispatch(context.Background(), &RequestCtx{Now: time.Unix(1714000000, 0)},
		&tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS, Payload: nil})
	require.Equal(t, tbproto.RpcStatus_RPC_STATUS_OK, resp.Status)

	var list tbproto.BootstrapPeerList
	require.NoError(t, proto.Unmarshal(resp.Payload, &list))
	require.NoError(t, tbproto.ValidateBootstrapPeerList(&list))
	require.Equal(t, svc.issuerID[:], list.IssuerId)
	require.Equal(t, uint64(1714000000), list.SignedAt)
	require.Equal(t, uint64(1714000600), list.ExpiresAt)
	require.Len(t, list.Peers, 2)
	require.NoError(t, tbproto.VerifyBootstrapPeerListSig(svc.pub, &list))
}

func TestBootstrapPeers_FiltersSelf(t *testing.T) {
	svc := newFakeBootstrapService(t)
	svc.rows = []storage.KnownPeer{
		{TrackerID: svc.issuerID[:], Addr: "self.example:443", RegionHint: "self", HealthScore: 1.0, LastSeen: time.Unix(1713999000, 0), Source: "allowlist"},
		{TrackerID: bytes.Repeat([]byte{0x11}, 32), Addr: "b.example:443", RegionHint: "r2", HealthScore: 0.7, LastSeen: time.Unix(1713998000, 0), Source: "gossip"},
	}
	r := newRouterWithBootstrap(t, svc)
	resp := r.Dispatch(context.Background(), &RequestCtx{Now: time.Unix(1714000000, 0)},
		&tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS})
	require.Equal(t, tbproto.RpcStatus_RPC_STATUS_OK, resp.Status)
	var list tbproto.BootstrapPeerList
	require.NoError(t, proto.Unmarshal(resp.Payload, &list))
	require.Len(t, list.Peers, 1)
	require.Equal(t, "b.example:443", list.Peers[0].Addr)
}

func TestBootstrapPeers_TruncatesToMax(t *testing.T) {
	svc := newFakeBootstrapService(t)
	svc.maxPeers = 3
	svc.rows = make([]storage.KnownPeer, 10)
	for i := range svc.rows {
		svc.rows[i] = storage.KnownPeer{
			TrackerID: bytes.Repeat([]byte{byte(0x20 + i)}, 32),
			Addr:      "x.example:443", RegionHint: "r",
			HealthScore: float64(10-i) / 10.0,
			LastSeen:    time.Unix(int64(1713999000+i), 0),
			Source:      "gossip",
		}
	}
	r := newRouterWithBootstrap(t, svc)
	resp := r.Dispatch(context.Background(), &RequestCtx{Now: time.Unix(1714000000, 0)},
		&tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS})
	require.Equal(t, tbproto.RpcStatus_RPC_STATUS_OK, resp.Status)
	var list tbproto.BootstrapPeerList
	require.NoError(t, proto.Unmarshal(resp.Payload, &list))
	require.Len(t, list.Peers, 3)
}

func TestBootstrapPeers_Empty(t *testing.T) {
	svc := newFakeBootstrapService(t)
	r := newRouterWithBootstrap(t, svc)
	resp := r.Dispatch(context.Background(), &RequestCtx{Now: time.Unix(1714000000, 0)},
		&tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS})
	require.Equal(t, tbproto.RpcStatus_RPC_STATUS_OK, resp.Status)
	var list tbproto.BootstrapPeerList
	require.NoError(t, proto.Unmarshal(resp.Payload, &list))
	require.Empty(t, list.Peers)
	require.NoError(t, tbproto.VerifyBootstrapPeerListSig(svc.pub, &list))
}

func TestBootstrapPeers_StorageError(t *testing.T) {
	svc := newFakeBootstrapService(t)
	svc.listErr = errors.New("disk on fire")
	r := newRouterWithBootstrap(t, svc)
	resp := r.Dispatch(context.Background(), &RequestCtx{Now: time.Unix(1714000000, 0)},
		&tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS})
	require.Equal(t, tbproto.RpcStatus_RPC_STATUS_INTERNAL, resp.Status)
	require.NotNil(t, resp.Error)
	require.Equal(t, "BOOTSTRAP_LIST_STORAGE", resp.Error.Code)
}

func TestBootstrapPeers_NilDeps(t *testing.T) {
	r, err := NewRouter(Deps{Logger: zerolog.Nop(), Now: func() time.Time { return time.Unix(1714000000, 0) }})
	require.NoError(t, err)
	resp := r.Dispatch(context.Background(), &RequestCtx{Now: time.Unix(1714000000, 0)},
		&tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS})
	require.Equal(t, tbproto.RpcStatus_RPC_STATUS_INTERNAL, resp.Status)
	require.NotNil(t, resp.Error)
	require.Equal(t, "NOT_IMPLEMENTED", resp.Error.Code)
}

package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

type realSigner struct {
	pub      ed25519.PublicKey
	priv     ed25519.PrivateKey
	issuer   ids.IdentityID
	store    *storage.Store
	maxPeers int
	ttl      time.Duration
}

func (a realSigner) ListKnownPeers(ctx context.Context, limit int, byHealthDesc bool) ([]storage.KnownPeer, error) {
	return a.store.ListKnownPeers(ctx, limit, byHealthDesc)
}
func (a realSigner) IssuerID() ids.IdentityID { return a.issuer }
func (a realSigner) Sign(canonical []byte) ([]byte, error) {
	return ed25519.Sign(a.priv, canonical), nil
}
func (a realSigner) MaxPeers() int      { return a.maxPeers }
func (a realSigner) TTL() time.Duration { return a.ttl }

func TestBootstrapPeers_Integration_RealStorage(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "tracker.db")
	store, err := storage.Open(context.Background(), dbPath)
	require.NoError(t, err)
	defer store.Close() //nolint:errcheck

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	issuer := ids.IdentityID(sha256.Sum256(pub))

	rows := []storage.KnownPeer{
		{TrackerID: bytes.Repeat([]byte{0x10}, 32), Addr: "a:443", RegionHint: "r1", HealthScore: 1.0, LastSeen: time.Unix(1714000000, 0), Source: "allowlist"},
		{TrackerID: bytes.Repeat([]byte{0x11}, 32), Addr: "b:443", RegionHint: "r2", HealthScore: 0.9, LastSeen: time.Unix(1714000000, 0), Source: "allowlist"},
		{TrackerID: bytes.Repeat([]byte{0x12}, 32), Addr: "c:443", RegionHint: "r3", HealthScore: 0.5, LastSeen: time.Unix(1714000000, 0), Source: "gossip"},
		{TrackerID: bytes.Repeat([]byte{0x13}, 32), Addr: "d:443", RegionHint: "r4", HealthScore: 0.3, LastSeen: time.Unix(1714000000, 0), Source: "gossip"},
	}
	for _, r := range rows {
		require.NoError(t, store.UpsertKnownPeer(context.Background(), r))
	}

	svc := realSigner{
		pub: pub, priv: priv, issuer: issuer, store: store,
		maxPeers: 50, ttl: 10 * time.Minute,
	}
	r, err := NewRouter(Deps{
		Logger:         zerolog.Nop(),
		Now:            func() time.Time { return time.Unix(1714000000, 0) },
		BootstrapPeers: svc,
	})
	require.NoError(t, err)
	resp := r.Dispatch(context.Background(), &RequestCtx{Now: time.Unix(1714000000, 0)},
		&tbproto.RpcRequest{Method: tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS})
	require.Equal(t, tbproto.RpcStatus_RPC_STATUS_OK, resp.Status)

	var list tbproto.BootstrapPeerList
	require.NoError(t, proto.Unmarshal(resp.Payload, &list))
	require.NoError(t, tbproto.ValidateBootstrapPeerList(&list))
	require.NoError(t, tbproto.VerifyBootstrapPeerListSig(pub, &list))

	require.Len(t, list.Peers, 4)
	require.Equal(t, "a:443", list.Peers[0].Addr)
	require.Equal(t, "b:443", list.Peers[1].Addr)
}

package federation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
	"google.golang.org/protobuf/proto"
)

// fakeKnownPeersArchive is an in-memory KnownPeersArchive for unit tests.
// It models last-write-wins by last_seen and allowlist pinning so emitter
// tests can rely on a faithful subset of storage semantics.
type fakeKnownPeersArchive struct {
	rows []storage.KnownPeer
}

func (f *fakeKnownPeersArchive) UpsertKnownPeer(_ context.Context, p storage.KnownPeer) error {
	for i, r := range f.rows {
		if bytes.Equal(r.TrackerID, p.TrackerID) {
			if p.LastSeen.After(r.LastSeen) || p.LastSeen.Equal(r.LastSeen) {
				if r.Source == "allowlist" && p.Source == "gossip" {
					p.Addr = r.Addr
					p.RegionHint = r.RegionHint
					p.Source = "allowlist"
				}
				f.rows[i] = p
			}
			return nil
		}
	}
	f.rows = append(f.rows, p)
	return nil
}

func (f *fakeKnownPeersArchive) GetKnownPeer(_ context.Context, trackerID []byte) (storage.KnownPeer, bool, error) {
	for _, r := range f.rows {
		if bytes.Equal(r.TrackerID, trackerID) {
			return r, true, nil
		}
	}
	return storage.KnownPeer{}, false, nil
}

func (f *fakeKnownPeersArchive) ListKnownPeers(_ context.Context, limit int, _ bool) ([]storage.KnownPeer, error) {
	if limit > len(f.rows) {
		limit = len(f.rows)
	}
	return append([]storage.KnownPeer(nil), f.rows[:limit]...), nil
}

func mustGenKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, ids.TrackerID) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return pub, priv, ids.TrackerID(sha256.Sum256(pub))
}

func TestPeerExchange_EmitNow_BuildsPeerExchangeFromArchive(t *testing.T) {
	_, myPriv, myID := mustGenKeypair(t)

	bID := bytes.Repeat([]byte{0xBB}, 32)
	cID := bytes.Repeat([]byte{0xCC}, 32)
	arch := &fakeKnownPeersArchive{rows: []storage.KnownPeer{
		{TrackerID: bID, Addr: "wss://b:443", LastSeen: time.Unix(100, 0), RegionHint: "eu", HealthScore: 0.0, Source: "allowlist"},
		{TrackerID: cID, Addr: "wss://c:443", LastSeen: time.Unix(50, 0), RegionHint: "us", HealthScore: 0.7, Source: "gossip"},
	}}

	var (
		gotKind    fed.Kind
		gotPayload []byte
	)
	forward := func(_ context.Context, kind fed.Kind, payload []byte) {
		gotKind = kind
		gotPayload = payload
	}

	emitted := 0
	pc := newPeerExchangeCoordinator(peerExchangeCoordinatorCfg{
		MyTrackerID:   myID,
		MyPriv:        myPriv,
		Archive:       arch,
		Forward:       forward,
		PeerConnected: func(id ids.TrackerID) bool { return bytes.Equal(id[:], bID) },
		Now:           func() time.Time { return time.Unix(1000, 0) },
		EmitCap:       100,
		Invalid:       func(string) {},
		OnEmit:        func() { emitted++ },
		OnReceived:    func(string) {},
	})

	require.NoError(t, pc.EmitNow(context.Background()))
	require.Equal(t, 1, emitted, "OnEmit fired")
	require.Equal(t, fed.Kind_KIND_PEER_EXCHANGE, gotKind)

	var msg fed.PeerExchange
	require.NoError(t, proto.Unmarshal(gotPayload, &msg))
	require.Len(t, msg.Peers, 2)

	byID := map[string]*fed.KnownPeer{}
	for _, p := range msg.Peers {
		byID[string(p.TrackerId)] = p
	}
	bEntry := byID[string(bID)]
	require.NotNil(t, bEntry)
	require.Equal(t, "wss://b:443", bEntry.Addr)
	require.InDelta(t, 1.0, bEntry.HealthScore, 0.0001, "allowlist + connected → 1.0")

	cEntry := byID[string(cID)]
	require.NotNil(t, cEntry)
	require.Equal(t, "wss://c:443", cEntry.Addr)
	require.InDelta(t, 0.7, cEntry.HealthScore, 0.0001, "gossip → verbatim")
}

func TestPeerExchange_EmitNow_AllowlistDisconnectedScores0_5(t *testing.T) {
	_, myPriv, myID := mustGenKeypair(t)
	bID := bytes.Repeat([]byte{0xBB}, 32)
	arch := &fakeKnownPeersArchive{rows: []storage.KnownPeer{
		{TrackerID: bID, Addr: "wss://b:443", LastSeen: time.Unix(100, 0), HealthScore: 0.0, Source: "allowlist"},
	}}
	var gotPayload []byte
	pc := newPeerExchangeCoordinator(peerExchangeCoordinatorCfg{
		MyTrackerID:   myID,
		MyPriv:        myPriv,
		Archive:       arch,
		Forward:       func(_ context.Context, _ fed.Kind, p []byte) { gotPayload = p },
		PeerConnected: func(_ ids.TrackerID) bool { return false },
		Now:           func() time.Time { return time.Unix(1000, 0) },
		EmitCap:       100,
		Invalid:       func(string) {}, OnEmit: func() {}, OnReceived: func(string) {},
	})
	require.NoError(t, pc.EmitNow(context.Background()))
	var msg fed.PeerExchange
	require.NoError(t, proto.Unmarshal(gotPayload, &msg))
	require.InDelta(t, 0.5, msg.Peers[0].HealthScore, 0.0001)
}

func TestPeerExchange_OnIncoming_UpsertsAllAndForwards(t *testing.T) {
	_, myPriv, myID := mustGenKeypair(t)
	arch := &fakeKnownPeersArchive{}

	bID := bytes.Repeat([]byte{0xBB}, 32)
	cID := bytes.Repeat([]byte{0xCC}, 32)
	msg := &fed.PeerExchange{
		Peers: []*fed.KnownPeer{
			{TrackerId: bID, Addr: "wss://b:443", LastSeen: 100, RegionHint: "eu", HealthScore: 0.6},
			{TrackerId: cID, Addr: "wss://c:443", LastSeen: 200, RegionHint: "us", HealthScore: 0.8},
		},
	}
	payload, err := proto.Marshal(msg)
	require.NoError(t, err)

	forwardCalls := 0
	pc := newPeerExchangeCoordinator(peerExchangeCoordinatorCfg{
		MyTrackerID:   myID,
		MyPriv:        myPriv,
		Archive:       arch,
		Forward:       func(_ context.Context, _ fed.Kind, _ []byte) { forwardCalls++ },
		PeerConnected: func(_ ids.TrackerID) bool { return false },
		Now:           func() time.Time { return time.Unix(1000, 0) },
		EmitCap:       100,
		Invalid:       func(string) {},
		OnEmit:        func() {},
		OnReceived:    func(_ string) {},
	})

	env := &fed.Envelope{Kind: fed.Kind_KIND_PEER_EXCHANGE, Payload: payload}
	pc.OnIncoming(context.Background(), env, msg)

	require.Len(t, arch.rows, 2)
	for _, r := range arch.rows {
		require.Equal(t, "gossip", r.Source)
	}
	require.Equal(t, 1, forwardCalls)
}

func TestPeerExchange_OnIncoming_SkipsSelfEntry(t *testing.T) {
	_, myPriv, myID := mustGenKeypair(t)
	arch := &fakeKnownPeersArchive{}
	myBytes := myID[:]

	msg := &fed.PeerExchange{
		Peers: []*fed.KnownPeer{
			{TrackerId: myBytes, Addr: "wss://self:443", LastSeen: 100, HealthScore: 0.5},
			{TrackerId: bytes.Repeat([]byte{0xBB}, 32), Addr: "wss://b:443", LastSeen: 100, HealthScore: 0.6},
		},
	}
	payload, _ := proto.Marshal(msg)
	pc := newPeerExchangeCoordinator(peerExchangeCoordinatorCfg{
		MyTrackerID:   myID,
		MyPriv:        myPriv,
		Archive:       arch,
		Forward:       func(_ context.Context, _ fed.Kind, _ []byte) {},
		PeerConnected: func(_ ids.TrackerID) bool { return false },
		Now:           func() time.Time { return time.Unix(1000, 0) },
		EmitCap:       100,
		Invalid:       func(string) {}, OnEmit: func() {}, OnReceived: func(string) {},
	})
	pc.OnIncoming(context.Background(), &fed.Envelope{Payload: payload}, msg)

	require.Len(t, arch.rows, 1, "self entry skipped")
	require.NotEqual(t, myBytes, arch.rows[0].TrackerID)
}

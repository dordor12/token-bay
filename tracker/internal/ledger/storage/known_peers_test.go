package storage

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestKnownPeers_UpsertAndGet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	id := bytes.Repeat([]byte{1}, 32)
	row := KnownPeer{
		TrackerID:   id,
		Addr:        "wss://a.example:443",
		LastSeen:    time.Unix(1714000000, 0),
		RegionHint:  "eu-central-1",
		HealthScore: 0.9,
		Source:      "allowlist",
	}
	require.NoError(t, s.UpsertKnownPeer(ctx, row))

	got, ok, err := s.GetKnownPeer(ctx, id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "wss://a.example:443", got.Addr)
	require.Equal(t, "eu-central-1", got.RegionHint)
	require.InDelta(t, 0.9, got.HealthScore, 0.0001)
	require.Equal(t, "allowlist", got.Source)
	require.Equal(t, int64(1714000000), got.LastSeen.Unix())
}

func TestKnownPeers_GetMissReturnsFalse(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	_, ok, err := s.GetKnownPeer(ctx, bytes.Repeat([]byte{9}, 32))
	require.NoError(t, err)
	require.False(t, ok)
}

func TestKnownPeers_LastWriteWins(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := bytes.Repeat([]byte{1}, 32)

	// Insert at t=200.
	require.NoError(t, s.UpsertKnownPeer(ctx, KnownPeer{
		TrackerID: id, Addr: "wss://a:1", LastSeen: time.Unix(200, 0),
		HealthScore: 0.9, Source: "gossip",
	}))
	// Stale upsert at t=100 with different addr — must NOT overwrite.
	require.NoError(t, s.UpsertKnownPeer(ctx, KnownPeer{
		TrackerID: id, Addr: "wss://b:2", LastSeen: time.Unix(100, 0),
		HealthScore: 0.1, Source: "gossip",
	}))
	got, _, err := s.GetKnownPeer(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "wss://a:1", got.Addr)
	require.InDelta(t, 0.9, got.HealthScore, 0.0001)
	require.Equal(t, int64(200), got.LastSeen.Unix())

	// Newer upsert at t=300 — wins.
	require.NoError(t, s.UpsertKnownPeer(ctx, KnownPeer{
		TrackerID: id, Addr: "wss://c:3", LastSeen: time.Unix(300, 0),
		HealthScore: 0.5, Source: "gossip",
	}))
	got, _, err = s.GetKnownPeer(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "wss://c:3", got.Addr)
	require.InDelta(t, 0.5, got.HealthScore, 0.0001)
}

func TestKnownPeers_AllowlistPinning(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := bytes.Repeat([]byte{1}, 32)

	// Seed with allowlist row.
	require.NoError(t, s.UpsertKnownPeer(ctx, KnownPeer{
		TrackerID: id, Addr: "wss://config:443", LastSeen: time.Unix(100, 0),
		RegionHint: "operator-config-region", HealthScore: 0.5, Source: "allowlist",
	}))

	// Gossip says different addr/region/health at later time — addr +
	// region_hint must NOT change; health_score MAY update; source stays
	// 'allowlist'.
	require.NoError(t, s.UpsertKnownPeer(ctx, KnownPeer{
		TrackerID: id, Addr: "wss://gossip-lie:1", LastSeen: time.Unix(200, 0),
		RegionHint: "lie", HealthScore: 0.99, Source: "gossip",
	}))
	got, _, err := s.GetKnownPeer(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "wss://config:443", got.Addr, "allowlist addr must be pinned")
	require.Equal(t, "operator-config-region", got.RegionHint, "allowlist region_hint must be pinned")
	require.Equal(t, "allowlist", got.Source, "source must not change to gossip")
	require.InDelta(t, 0.99, got.HealthScore, 0.0001, "health_score may update")
}

func TestKnownPeers_AllowlistRefreshFromConfig(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id := bytes.Repeat([]byte{1}, 32)

	// Seed.
	require.NoError(t, s.UpsertKnownPeer(ctx, KnownPeer{
		TrackerID: id, Addr: "wss://old:443", LastSeen: time.Unix(100, 0),
		RegionHint: "old-region", HealthScore: 0.5, Source: "allowlist",
	}))
	// Operator changed YAML config: allowlist→allowlist write must take effect.
	require.NoError(t, s.UpsertKnownPeer(ctx, KnownPeer{
		TrackerID: id, Addr: "wss://new:443", LastSeen: time.Unix(200, 0),
		RegionHint: "new-region", HealthScore: 0.5, Source: "allowlist",
	}))
	got, _, err := s.GetKnownPeer(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "wss://new:443", got.Addr)
	require.Equal(t, "new-region", got.RegionHint)
}

func TestKnownPeers_ListByHealthDescDeterministic(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	idA := bytes.Repeat([]byte{0xAA}, 32)
	idB := bytes.Repeat([]byte{0xBB}, 32)
	idC := bytes.Repeat([]byte{0xCC}, 32)
	require.NoError(t, s.UpsertKnownPeer(ctx, KnownPeer{TrackerID: idC, Addr: "x", LastSeen: time.Unix(1, 0), HealthScore: 0.5, Source: "gossip"}))
	require.NoError(t, s.UpsertKnownPeer(ctx, KnownPeer{TrackerID: idA, Addr: "x", LastSeen: time.Unix(1, 0), HealthScore: 0.9, Source: "gossip"}))
	require.NoError(t, s.UpsertKnownPeer(ctx, KnownPeer{TrackerID: idB, Addr: "x", LastSeen: time.Unix(1, 0), HealthScore: 0.5, Source: "gossip"}))

	got, err := s.ListKnownPeers(ctx, 10, true)
	require.NoError(t, err)
	require.Len(t, got, 3)
	require.Equal(t, idA, got[0].TrackerID, "highest health first")
	require.Equal(t, idB, got[1].TrackerID, "tie broken by tracker_id ASC")
	require.Equal(t, idC, got[2].TrackerID)
}

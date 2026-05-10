package bootstrap_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/bootstrap"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
	"github.com/token-bay/token-bay/shared/ids"
)

func entry(seed byte, addr string, score float64, lastSeen time.Time) trackerclient.BootstrapPeer {
	var id ids.IdentityID
	for i := range id {
		id[i] = seed
	}
	return trackerclient.BootstrapPeer{
		TrackerID:   id,
		Addr:        addr,
		HealthScore: score,
		LastSeen:    lastSeen,
	}
}

// entryN builds an entry with a TrackerID derived from a uint16 index so
// 300+ unique non-zero IDs are easy to generate (single-byte fills wrap).
func entryN(idx uint16, addr string, score float64, lastSeen time.Time) trackerclient.BootstrapPeer {
	var id ids.IdentityID
	id[0] = byte((idx >> 8) & 0xff)
	id[1] = byte(idx & 0xff)
	id[31] = 0x01 // ensure non-zero even when idx == 0
	return trackerclient.BootstrapPeer{
		TrackerID:   id,
		Addr:        addr,
		HealthScore: score,
		LastSeen:    lastSeen,
	}
}

func newStore(t *testing.T, now time.Time) (*bootstrap.KnownPeers, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "known_peers.json")
	store, err := bootstrap.LoadKnownPeers(path, func() time.Time { return now })
	require.NoError(t, err)
	return store, path
}

func TestKnownPeers_LoadsEmptyWhenAbsent(t *testing.T) {
	store, _ := newStore(t, time.Unix(1_800_000_000, 0))
	require.Equal(t, 0, store.Len())
	require.Empty(t, store.Snapshot())
}

func TestKnownPeers_MergeAddsAndPersists(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	store, path := newStore(t, now)

	peers := []trackerclient.BootstrapPeer{
		entry(0x01, "a.example.org:8443", 0.9, now.Add(-time.Hour)),
		entry(0x02, "b.example.org:8443", 0.4, now.Add(-time.Minute)),
	}
	require.NoError(t, store.Merge(context.Background(), peers))
	require.Equal(t, 2, store.Len())

	snap := store.Snapshot()
	require.Len(t, snap, 2)
	require.Equal(t, "a.example.org:8443", snap[0].Addr, "highest health first")

	// Persisted to disk.
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var blob struct {
		Peers []map[string]any `json:"peers"`
	}
	require.NoError(t, json.Unmarshal(data, &blob))
	require.Len(t, blob.Peers, 2)
}

func TestKnownPeers_MergeIsIdempotent(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	store, _ := newStore(t, now)

	peers := []trackerclient.BootstrapPeer{
		entry(0x01, "a.example.org:8443", 0.9, now.Add(-time.Minute)),
		entry(0x02, "b.example.org:8443", 0.5, now.Add(-time.Minute)),
	}
	require.NoError(t, store.Merge(context.Background(), peers))
	require.NoError(t, store.Merge(context.Background(), peers))
	require.NoError(t, store.Merge(context.Background(), peers))

	require.Equal(t, 2, store.Len())
	snap := store.Snapshot()
	require.Len(t, snap, 2)
	require.Equal(t, "a.example.org:8443", snap[0].Addr)
}

func TestKnownPeers_MergeLastWriteWinsByLastSeen(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	store, _ := newStore(t, now)

	earlier := entry(0x01, "old.example.org:8443", 0.9, now.Add(-2*time.Hour))
	later := entry(0x01, "new.example.org:8443", 0.4, now.Add(-time.Minute))

	require.NoError(t, store.Merge(context.Background(), []trackerclient.BootstrapPeer{earlier}))
	require.NoError(t, store.Merge(context.Background(), []trackerclient.BootstrapPeer{later}))

	snap := store.Snapshot()
	require.Len(t, snap, 1)
	require.Equal(t, "new.example.org:8443", snap[0].Addr, "newer last_seen wins")
	require.InDelta(t, 0.4, snap[0].HealthScore, 0.001)

	// Stale write must NOT regress.
	require.NoError(t, store.Merge(context.Background(), []trackerclient.BootstrapPeer{earlier}))
	snap = store.Snapshot()
	require.Equal(t, "new.example.org:8443", snap[0].Addr)
}

func TestKnownPeers_MergeSkipsStaleEntries(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	store, _ := newStore(t, now)

	stale := entry(0x01, "stale.example.org:8443", 0.9, now.Add(-8*24*time.Hour))
	fresh := entry(0x02, "fresh.example.org:8443", 0.6, now.Add(-1*time.Hour))

	require.NoError(t, store.Merge(context.Background(), []trackerclient.BootstrapPeer{stale, fresh}))
	snap := store.Snapshot()
	require.Len(t, snap, 1, "rows older than 7d should not be archived")
	require.Equal(t, "fresh.example.org:8443", snap[0].Addr)
}

func TestKnownPeers_MergeCapsAt256ByHealth(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	store, _ := newStore(t, now)

	// 300 entries with descending health, all fresh.
	entries := make([]trackerclient.BootstrapPeer, 300)
	for i := range 300 {
		entries[i] = entryN(uint16(i), fmt.Sprintf("t%03d.example.org:8443", i), float64(300-i)/300.0, now.Add(-time.Minute))
	}
	require.NoError(t, store.Merge(context.Background(), entries))

	require.Equal(t, 256, store.Len(), "store must cap at 256 by health_score")
	snap := store.Snapshot()
	require.Len(t, snap, 256)
	// Top entry should be the highest-health (i=0) and the cutoff is the
	// 256th (i=255). The 257th-best (i=256) must be evicted.
	require.Equal(t, "t000.example.org:8443", snap[0].Addr)
	for _, p := range snap {
		require.GreaterOrEqual(t, p.HealthScore, float64(45)/300.0,
			"entries below the 256th health rank must be evicted")
	}
}

func TestKnownPeers_PersistAndReload(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	dir := t.TempDir()
	path := filepath.Join(dir, "known_peers.json")

	store1, err := bootstrap.LoadKnownPeers(path, func() time.Time { return now })
	require.NoError(t, err)
	require.NoError(t, store1.Merge(context.Background(), []trackerclient.BootstrapPeer{
		entry(0x01, "a.example.org:8443", 0.9, now.Add(-time.Minute)),
		entry(0x02, "b.example.org:8443", 0.5, now.Add(-time.Minute)),
	}))

	// Reload from disk under a fresh store; all entries (still within 7d)
	// must re-appear.
	store2, err := bootstrap.LoadKnownPeers(path, func() time.Time { return now })
	require.NoError(t, err)
	snap := store2.Snapshot()
	require.Len(t, snap, 2)
	require.Equal(t, "a.example.org:8443", snap[0].Addr)
}

func TestKnownPeers_ReloadDropsStaleRows(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	dir := t.TempDir()
	path := filepath.Join(dir, "known_peers.json")

	// Seed a file with one row whose last_seen is 8 days old.
	old := now.Add(-8 * 24 * time.Hour)
	store1, err := bootstrap.LoadKnownPeers(path, func() time.Time { return old })
	require.NoError(t, err)
	require.NoError(t, store1.Merge(context.Background(), []trackerclient.BootstrapPeer{
		entry(0x01, "stale.example.org:8443", 0.9, old.Add(-time.Minute)),
	}))

	store2, err := bootstrap.LoadKnownPeers(path, func() time.Time { return now })
	require.NoError(t, err)
	require.Empty(t, store2.Snapshot(), "stale rows should not survive reload")
}

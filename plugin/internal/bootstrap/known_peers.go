package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
	"github.com/token-bay/token-bay/shared/ids"
)

// KnownPeersCap is the maximum number of rows kept in known_peers.json.
// Federation §7 expects each peer's known-peers table to grow toward
// network membership; the plugin caps locally at 256 by health_score.
const KnownPeersCap = 256

// KnownPeersStaleAge is the cut-off after which a row is considered
// stale and dropped on Merge / Load.
const KnownPeersStaleAge = 7 * 24 * time.Hour

// KnownPeers is a thread-safe in-memory store of peer entries learned
// via federation peer-exchange gossip and (occasionally) freshly-fetched
// signed bootstrap lists. Mutations are persisted atomically to a JSON
// file under $TOKEN_BAY_HOME/known_peers.json.
//
// Concurrency: all public methods are safe for concurrent use.
type KnownPeers struct {
	path string
	now  func() time.Time

	mu    sync.Mutex
	peers map[ids.IdentityID]trackerclient.BootstrapPeer
}

type persistedFile struct {
	Peers []persistedPeer `json:"peers"`
}

type persistedPeer struct {
	TrackerID   string  `json:"tracker_id"` // hex
	Addr        string  `json:"addr"`
	RegionHint  string  `json:"region_hint,omitempty"`
	HealthScore float64 `json:"health_score"`
	LastSeen    int64   `json:"last_seen"` // unix seconds
}

// LoadKnownPeers opens the store at path. If the file does not exist,
// the store starts empty and creates the file on the first Merge. Stale
// rows (older than KnownPeersStaleAge relative to nowFn()) are dropped
// on load.
func LoadKnownPeers(path string, nowFn func() time.Time) (*KnownPeers, error) {
	if path == "" {
		return nil, errors.New("bootstrap: KnownPeers path empty")
	}
	if nowFn == nil {
		nowFn = time.Now
	}
	store := &KnownPeers{
		path:  path,
		now:   nowFn,
		peers: make(map[ids.IdentityID]trackerclient.BootstrapPeer),
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return store, nil
		}
		return nil, fmt.Errorf("bootstrap: read known_peers: %w", err)
	}
	var blob persistedFile
	if err := json.Unmarshal(raw, &blob); err != nil {
		return nil, fmt.Errorf("bootstrap: parse known_peers: %w", err)
	}
	now := nowFn()
	for _, p := range blob.Peers {
		entry, ok := decodePersisted(p)
		if !ok {
			continue
		}
		if now.Sub(entry.LastSeen) > KnownPeersStaleAge {
			continue
		}
		store.peers[entry.TrackerID] = entry
	}
	return store, nil
}

// Merge upserts entries with last-write-wins by LastSeen, drops rows
// older than KnownPeersStaleAge, caps the store at KnownPeersCap by
// HealthScore (descending), and atomically persists.
func (k *KnownPeers) Merge(ctx context.Context, entries []trackerclient.BootstrapPeer) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	now := k.now()
	cutoff := now.Add(-KnownPeersStaleAge)

	k.mu.Lock()
	defer k.mu.Unlock()

	for _, e := range entries {
		if e.TrackerID == (ids.IdentityID{}) || e.Addr == "" {
			continue
		}
		if e.LastSeen.Before(cutoff) {
			continue
		}
		existing, ok := k.peers[e.TrackerID]
		if ok && existing.LastSeen.After(e.LastSeen) {
			continue // stale write — don't regress
		}
		k.peers[e.TrackerID] = e
	}

	// Drop newly-stale rows from prior loads (e.g., when nowFn advanced).
	for id, p := range k.peers {
		if p.LastSeen.Before(cutoff) {
			delete(k.peers, id)
		}
	}

	// Cap at KnownPeersCap by HealthScore desc.
	if len(k.peers) > KnownPeersCap {
		all := make([]trackerclient.BootstrapPeer, 0, len(k.peers))
		for _, p := range k.peers {
			all = append(all, p)
		}
		sortByHealthDesc(all)
		evicted := all[KnownPeersCap:]
		for _, p := range evicted {
			delete(k.peers, p.TrackerID)
		}
	}

	return k.persistLocked()
}

// Snapshot returns the entries sorted by HealthScore descending. The
// returned slice is a copy — callers may mutate it freely.
func (k *KnownPeers) Snapshot() []trackerclient.BootstrapPeer {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]trackerclient.BootstrapPeer, 0, len(k.peers))
	for _, p := range k.peers {
		out = append(out, p)
	}
	sortByHealthDesc(out)
	return out
}

// Len returns the number of entries currently in the store.
func (k *KnownPeers) Len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.peers)
}

// HandlePeerExchange implements trackerclient.PeerExchangeHandler. It
// is a thin adapter that delegates to Merge so the store can be passed
// directly as Config.PeerExchangeHandler. The dispatcher passes a Ctx
// only checked for cancellation, and Merge performs its own write.
func (k *KnownPeers) HandlePeerExchange(ctx trackerclient.Ctx, entries []trackerclient.BootstrapPeer) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return k.Merge(context.Background(), entries)
}

func sortByHealthDesc(peers []trackerclient.BootstrapPeer) {
	sort.SliceStable(peers, func(i, j int) bool {
		if peers[i].HealthScore != peers[j].HealthScore {
			return peers[i].HealthScore > peers[j].HealthScore
		}
		// Deterministic tiebreak by TrackerID bytes.
		for b := range peers[i].TrackerID {
			if peers[i].TrackerID[b] != peers[j].TrackerID[b] {
				return peers[i].TrackerID[b] < peers[j].TrackerID[b]
			}
		}
		return false
	})
}

// persistLocked must be called with k.mu held.
func (k *KnownPeers) persistLocked() error {
	rows := make([]persistedPeer, 0, len(k.peers))
	for _, p := range k.peers {
		rows = append(rows, encodePersisted(p))
	}
	// Stable order on disk (highest health first) so byte-equal writes
	// produce byte-equal files.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].HealthScore != rows[j].HealthScore {
			return rows[i].HealthScore > rows[j].HealthScore
		}
		return rows[i].TrackerID < rows[j].TrackerID
	})
	data, err := json.MarshalIndent(persistedFile{Peers: rows}, "", "  ")
	if err != nil {
		return fmt.Errorf("bootstrap: marshal known_peers: %w", err)
	}
	return atomicWriteFile(k.path, data, 0o600)
}

// SignedListPath returns the conventional bootstrap.signed path under the
// same directory as known_peers.json.
func (k *KnownPeers) SignedListPath() string {
	return filepath.Join(filepath.Dir(k.path), "bootstrap.signed")
}

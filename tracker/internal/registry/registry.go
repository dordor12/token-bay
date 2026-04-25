package registry

import (
	"encoding/binary"
	"net/netip"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// DefaultShardCount is the registry's default shard count when callers want a
// reasonable default rather than tuning it themselves. 16 strikes a balance
// between contention scattering and per-shard overhead for the v1 capacity
// target (≤ 10³ concurrent seeders per spec §6).
const DefaultShardCount = 16

// Registry is a sharded, in-memory store of SeederRecords. Safe for
// concurrent use. See package doc for the concurrency model.
type Registry struct {
	shards []*shard
}

// New returns a Registry with numShards shards. Returns ErrInvalidShardCount
// when numShards <= 0.
func New(numShards int) (*Registry, error) {
	if numShards <= 0 {
		return nil, ErrInvalidShardCount
	}
	r := &Registry{shards: make([]*shard, numShards)}
	for i := range r.shards {
		r.shards[i] = newShard()
	}
	return r, nil
}

// NumShards returns the registry's shard count. Useful for diagnostics and
// tests; callers should not depend on a specific value.
func (r *Registry) NumShards() int { return len(r.shards) }

// shardIndex returns the shard index for an IdentityID. The first eight bytes
// of the ID are interpreted as a big-endian uint64 and reduced modulo the
// shard count. Identity IDs are Ed25519-pubkey-derived hashes so the leading
// bytes are uniformly distributed; a more elaborate hash would be wasted work.
func (r *Registry) shardIndex(id ids.IdentityID) int {
	b := id.Bytes()
	return int(binary.BigEndian.Uint64(b[:8]) % uint64(len(r.shards)))
}

func (r *Registry) shardFor(id ids.IdentityID) *shard {
	return r.shards[r.shardIndex(id)]
}

// Register inserts or replaces a SeederRecord. Caller is responsible for
// providing the full record — including LastHeartbeat. Idempotent upsert.
func (r *Registry) Register(rec SeederRecord) {
	r.shardFor(rec.IdentityID).put(rec)
}

// Get returns a deep copy of the seeder's record. ok is false when no record
// exists for id.
func (r *Registry) Get(id ids.IdentityID) (SeederRecord, bool) {
	return r.shardFor(id).get(id)
}

// Deregister removes the seeder. No-op when no record exists.
func (r *Registry) Deregister(id ids.IdentityID) {
	r.shardFor(id).delete(id)
}

// Heartbeat sets LastHeartbeat to now. Returns ErrUnknownSeeder if the seeder
// is not in the registry.
func (r *Registry) Heartbeat(id ids.IdentityID, now time.Time) error {
	return r.shardFor(id).update(id, func(rec *SeederRecord) error {
		rec.LastHeartbeat = now
		return nil
	})
}

// UpdateExternalAddr sets the seeder's reflexive (STUN-observed) address.
// Other NetCoords fields are preserved. Returns ErrUnknownSeeder if the
// seeder is not in the registry.
func (r *Registry) UpdateExternalAddr(id ids.IdentityID, addr netip.AddrPort) error {
	return r.shardFor(id).update(id, func(rec *SeederRecord) error {
		rec.NetCoords.ExternalAddr = addr
		return nil
	})
}

// Advertise atomically applies the seeder's reported capabilities,
// availability, and headroom. headroom must be within [0.0, 1.0] or
// ErrInvalidHeadroom is returned (and no mutation occurs). Returns
// ErrUnknownSeeder if the seeder is not in the registry.
func (r *Registry) Advertise(id ids.IdentityID, caps Capabilities, available bool, headroom float64) error {
	if headroom < 0.0 || headroom > 1.0 {
		return ErrInvalidHeadroom
	}
	return r.shardFor(id).update(id, func(rec *SeederRecord) error {
		rec.Capabilities = caps
		rec.Available = available
		rec.HeadroomEstimate = headroom
		return nil
	})
}

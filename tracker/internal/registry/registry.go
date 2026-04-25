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
// availability, and headroom. headroom must be a real number within
// [0.0, 1.0] (NaN is rejected) or ErrInvalidHeadroom is returned and no
// mutation occurs. Returns ErrUnknownSeeder if the seeder is not in the
// registry. caps is deep-copied into the store; the caller may safely
// mutate the passed Capabilities after the call returns.
func (r *Registry) Advertise(id ids.IdentityID, caps Capabilities, available bool, headroom float64) error {
	// !(in-range) rather than (out-of-range) so NaN is rejected too — NaN
	// compares false to every numeric ordering.
	if !(headroom >= 0.0 && headroom <= 1.0) {
		return ErrInvalidHeadroom
	}
	stored := cloneCapabilities(caps)
	return r.shardFor(id).update(id, func(rec *SeederRecord) error {
		rec.Capabilities = stored
		rec.Available = available
		rec.HeadroomEstimate = headroom
		return nil
	})
}

// UpdateReputation sets the seeder's reputation score. The registry does not
// constrain the score range — that is the reputation subsystem's contract.
// Returns ErrUnknownSeeder if the seeder is not in the registry.
func (r *Registry) UpdateReputation(id ids.IdentityID, score float64) error {
	return r.shardFor(id).update(id, func(rec *SeederRecord) error {
		rec.ReputationScore = score
		return nil
	})
}

// IncLoad increments the seeder's in-flight offer count by one. Returns the
// new load value. ErrUnknownSeeder if the seeder is not in the registry.
func (r *Registry) IncLoad(id ids.IdentityID) (int, error) {
	var newLoad int
	err := r.shardFor(id).update(id, func(rec *SeederRecord) error {
		rec.Load++
		newLoad = rec.Load
		return nil
	})
	if err != nil {
		return 0, err
	}
	return newLoad, nil
}

// DecLoad decrements the seeder's in-flight offer count by one. Returns the
// new load value. ErrLoadUnderflow is returned (and no mutation occurs) if
// load is already 0. ErrUnknownSeeder if the seeder is not in the registry.
func (r *Registry) DecLoad(id ids.IdentityID) (int, error) {
	var newLoad int
	err := r.shardFor(id).update(id, func(rec *SeederRecord) error {
		if rec.Load <= 0 {
			return ErrLoadUnderflow
		}
		rec.Load--
		newLoad = rec.Load
		return nil
	})
	if err != nil {
		return 0, err
	}
	return newLoad, nil
}

// Snapshot returns a deep copy of every record currently in the registry.
// Order is unspecified. Each shard is briefly RLocked in turn — for very large
// registries the result is a near-consistent (not strictly atomic) view across
// shards, which is acceptable for the broker's selection workload.
func (r *Registry) Snapshot() []SeederRecord {
	var out []SeederRecord
	for _, sh := range r.shards {
		out = append(out, sh.snapshot()...)
	}
	return out
}

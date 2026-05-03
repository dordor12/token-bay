package admission

import (
	"encoding/binary"
	"sync"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// rollingWindowDays is the §4.2 30-day window. Each ConsumerCreditState
// keeps three arrays of this length (settlements, disputes, flow). The
// AdmissionConfig field RollingWindowDays carries this value at runtime;
// the constant here is the array fixed-size dimension.
const rollingWindowDays = 30

// MinuteBucket is the one-minute cell in the seeder heartbeat-reliability
// rolling window. Expected = how many heartbeats we should have received
// at the configured cadence; Actual = how many we did.
type MinuteBucket struct {
	Expected uint32
	Actual   uint32
}

// heartbeatWindowMinutes is the §4.2 10-minute window dimension.
const heartbeatWindowMinutes = 10

// ConsumerCreditState is admission's per-consumer credit history. Held
// only in memory in this revision; persistence lands in plan 3.
type ConsumerCreditState struct {
	FirstSeenAt time.Time

	// SettlementBuckets — Total = total settlements that day, A = clean count.
	SettlementBuckets [rollingWindowDays]DayBucket

	// DisputeBuckets — A = filed count, B = upheld count.
	DisputeBuckets [rollingWindowDays]DayBucket

	// FlowBuckets — A = credits earned (settled to this consumer as seeder),
	// B = credits spent (settled away).
	FlowBuckets [rollingWindowDays]DayBucket

	// LastBalanceSeen mirrors the ledger's authoritative balance projection.
	// Allowed to drift on missed events (recovered via §9.1
	// /admission/recompute/<consumer_id> in plan 3).
	LastBalanceSeen int64
}

// SeederHeartbeatState tracks the rolling heartbeat-reliability window plus
// the most recent headroom signal from this seeder.
type SeederHeartbeatState struct {
	Buckets              [heartbeatWindowMinutes]MinuteBucket
	LastBucketRollAt     time.Time
	LastHeadroomEstimate uint32
	LastHeadroomSource   uint8 // 0=unspecified, 1=heuristic, 2=usage_probe (mirrors admission.TickSource)
	LastHeadroomTs       time.Time
	CanProbeUsage        bool
}

// consumerShard owns a fraction of the consumer credit-state map. Each
// shard is independently locked. See package doc concurrency model.
type consumerShard struct {
	mu sync.RWMutex
	m  map[ids.IdentityID]*ConsumerCreditState
}

// newConsumerShard allocates an empty, ready-to-use consumer shard.
func newConsumerShard() *consumerShard {
	return &consumerShard{m: make(map[ids.IdentityID]*ConsumerCreditState)}
}

// getOrInit returns the consumer's state, creating it on first use with
// FirstSeenAt = now. Holds the write lock only on the create path.
func (sh *consumerShard) getOrInit(id ids.IdentityID, now time.Time) *ConsumerCreditState {
	sh.mu.RLock()
	st, ok := sh.m[id]
	sh.mu.RUnlock()
	if ok {
		return st
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	// Re-check under write lock — another goroutine may have created it.
	if st, ok := sh.m[id]; ok {
		return st
	}
	st = &ConsumerCreditState{FirstSeenAt: now}
	sh.m[id] = st
	return st
}

// get returns the existing state without creating one. ok is false when no
// state exists for id.
func (sh *consumerShard) get(id ids.IdentityID) (*ConsumerCreditState, bool) {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	st, ok := sh.m[id]
	return st, ok
}

// seederShard mirrors consumerShard for SeederHeartbeatState.
type seederShard struct {
	mu sync.RWMutex
	m  map[ids.IdentityID]*SeederHeartbeatState
}

// newSeederShard allocates an empty, ready-to-use seeder shard.
func newSeederShard() *seederShard {
	return &seederShard{m: make(map[ids.IdentityID]*SeederHeartbeatState)}
}

// getOrInit returns the seeder's heartbeat state, creating it on first use.
// LastBucketRollAt is initialized to now so the rolling window's first roll
// has a defined start.
func (sh *seederShard) getOrInit(id ids.IdentityID, now time.Time) *SeederHeartbeatState {
	sh.mu.RLock()
	st, ok := sh.m[id]
	sh.mu.RUnlock()
	if ok {
		return st
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if st, ok := sh.m[id]; ok {
		return st
	}
	st = &SeederHeartbeatState{LastBucketRollAt: now, LastHeadroomTs: now}
	sh.m[id] = st
	return st
}

// shardIndex returns the deterministic shard index for an IdentityID.
// Same scheme as registry.shardIndex (binary.BigEndian.Uint64 of the first
// 8 bytes mod n) so ID→shard routing is consistent across subsystems.
func shardIndex(id ids.IdentityID, n int) int {
	idx := int(binary.BigEndian.Uint64(id[:8]) % uint64(n)) //nolint:gosec // G115: bounded by n, a positive int
	return idx
}

// consumerShardFor routes an ID to its shard.
func consumerShardFor(shards []*consumerShard, id ids.IdentityID) *consumerShard {
	return shards[shardIndex(id, len(shards))]
}

// seederShardFor routes an ID to its shard.
func seederShardFor(shards []*seederShard, id ids.IdentityID) *seederShard {
	return shards[shardIndex(id, len(shards))]
}

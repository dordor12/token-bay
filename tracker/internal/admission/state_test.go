package admission

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConsumerShard_GetOrInit_CreatesOnFirstUse(t *testing.T) {
	sh := newConsumerShard()
	id := makeID(0x11)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	st := sh.getOrInit(id, now)
	require.NotNil(t, st)
	assert.True(t, st.FirstSeenAt.Equal(now), "FirstSeenAt initialized to now")
	assert.Equal(t, int64(0), st.LastBalanceSeen)
}

func TestConsumerShard_GetOrInit_ReturnsExisting(t *testing.T) {
	sh := newConsumerShard()
	id := makeID(0x11)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	first := sh.getOrInit(id, now)
	first.LastBalanceSeen = 12345

	later := now.Add(time.Hour)
	second := sh.getOrInit(id, later)
	assert.Same(t, first, second, "second call returns same pointer")
	assert.Equal(t, int64(12345), second.LastBalanceSeen)
	assert.True(t, second.FirstSeenAt.Equal(now), "FirstSeenAt unchanged after second call")
}

func TestConsumerShard_Get_ReportsAbsence(t *testing.T) {
	sh := newConsumerShard()
	_, ok := sh.get(makeID(0xFF))
	assert.False(t, ok)
}

func TestConsumerShards_RoutingByIdentityID(t *testing.T) {
	shards := newConsumerShards(16)
	require.Len(t, shards, 16)

	// Same ID → same shard.
	id := makeID(0x42)
	a := consumerShardFor(shards, id)
	b := consumerShardFor(shards, id)
	assert.Same(t, a, b)

	// Different IDs in different leading bytes likely route differently;
	// not strictly guaranteed for any specific pair (mod 16), but the
	// router's invariant is determinism, which we verify via same-ID call.
}

func TestSeederShard_GetOrInit_CreatesOnFirstUse(t *testing.T) {
	sh := newSeederShard()
	id := makeID(0xAA)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	st := sh.getOrInit(id, now)
	require.NotNil(t, st)
	assert.Equal(t, uint32(0), st.LastHeadroomEstimate)
	assert.False(t, st.LastHeadroomTs.IsZero(), "LastBucketRollAt initialized to now")
}

func TestSeederShard_GetOrInit_ReturnsExisting(t *testing.T) {
	sh := newSeederShard()
	id := makeID(0xAA)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	first := sh.getOrInit(id, now)
	first.LastHeadroomEstimate = 7500

	later := now.Add(time.Minute)
	second := sh.getOrInit(id, later)
	assert.Same(t, first, second)
	assert.Equal(t, uint32(7500), second.LastHeadroomEstimate)
}

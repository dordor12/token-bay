package registry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
)

func TestNew_ValidShardCount(t *testing.T) {
	r, err := New(8)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.Equal(t, 8, r.NumShards())
}

func TestNew_RejectsZeroShards(t *testing.T) {
	r, err := New(0)
	assert.Nil(t, r)
	assert.ErrorIs(t, err, ErrInvalidShardCount)
}

func TestNew_RejectsNegativeShards(t *testing.T) {
	r, err := New(-1)
	assert.Nil(t, r)
	assert.ErrorIs(t, err, ErrInvalidShardCount)
}

func TestRegistry_ShardFor_DeterministicForSameID(t *testing.T) {
	r, err := New(16)
	require.NoError(t, err)

	id := ids.IdentityID{0xDE, 0xAD, 0xBE, 0xEF, 0xCA, 0xFE, 0xBA, 0xBE}
	idx1 := r.shardIndex(id)
	idx2 := r.shardIndex(id)
	assert.Equal(t, idx1, idx2)
	assert.GreaterOrEqual(t, idx1, 0)
	assert.Less(t, idx1, 16)
}

func TestRegistry_ShardFor_DistributesAcrossShards(t *testing.T) {
	r, err := New(16)
	require.NoError(t, err)

	// Distinct IDs should end up in more than one shard. We don't assert a
	// distribution shape — just that 256 distinct IDs hit ≥ 4 shards (a very
	// loose hash-quality smoke check). We vary id[7] (the least significant
	// byte of the first 8-byte chunk) to produce actual distribution under
	// modulo-16 reduction.
	hits := make(map[int]struct{})
	for i := 0; i < 256; i++ {
		var id ids.IdentityID
		id[7] = byte(i)
		hits[r.shardIndex(id)] = struct{}{}
	}
	assert.GreaterOrEqual(t, len(hits), 4, "shardIndex should spread distinct IDs across shards")
}

func TestDefaultShardCount_IsPositive(t *testing.T) {
	assert.Greater(t, DefaultShardCount, 0)
}

func TestRegistry_RegisterThenGet(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	rec := SeederRecord{
		IdentityID: ids.IdentityID{0x42},
		Available:  true,
		Capabilities: Capabilities{
			Models: []string{"claude-opus-4-7"},
		},
	}
	r.Register(rec)

	got, ok := r.Get(rec.IdentityID)
	require.True(t, ok)
	assert.Equal(t, rec.IdentityID, got.IdentityID)
	assert.True(t, got.Available)
	assert.Equal(t, []string{"claude-opus-4-7"}, got.Capabilities.Models)
}

func TestRegistry_Get_Missing(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	_, ok := r.Get(ids.IdentityID{0xFF})
	assert.False(t, ok)
}

func TestRegistry_Register_Upserts(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	id := ids.IdentityID{0x11}
	r.Register(SeederRecord{IdentityID: id, Load: 1})
	r.Register(SeederRecord{IdentityID: id, Load: 9})

	got, ok := r.Get(id)
	require.True(t, ok)
	assert.Equal(t, 9, got.Load, "second Register call should overwrite first")
}

func TestRegistry_Deregister_Removes(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	id := ids.IdentityID{0x22}
	r.Register(SeederRecord{IdentityID: id})
	r.Deregister(id)

	_, ok := r.Get(id)
	assert.False(t, ok)
}

func TestRegistry_Deregister_MissingIsNoOp(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	assert.NotPanics(t, func() {
		r.Deregister(ids.IdentityID{0x99})
	})
}

func TestRegistry_Get_ReturnedSliceMutationDoesNotAffectStore(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	id := ids.IdentityID{0x33}
	r.Register(SeederRecord{
		IdentityID:   id,
		Capabilities: Capabilities{Models: []string{"claude-opus-4-7"}},
	})

	got, _ := r.Get(id)
	got.Capabilities.Models[0] = "INJECTED"

	got2, _ := r.Get(id)
	assert.Equal(t, "claude-opus-4-7", got2.Capabilities.Models[0],
		"store must not be aliased through returned record's slice")
}

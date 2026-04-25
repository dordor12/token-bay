package registry

import (
	"net/netip"
	"testing"
	"time"

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

func TestRegistry_Heartbeat_BumpsLastHeartbeat(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	id := ids.IdentityID{0x44}
	t0 := time.Date(2026, 4, 25, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(30 * time.Second)
	r.Register(SeederRecord{IdentityID: id, LastHeartbeat: t0})

	require.NoError(t, r.Heartbeat(id, t1))

	got, _ := r.Get(id)
	assert.Equal(t, t1, got.LastHeartbeat)
}

func TestRegistry_Heartbeat_UnknownReturnsErr(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	err = r.Heartbeat(ids.IdentityID{0xCC}, time.Now())
	assert.ErrorIs(t, err, ErrUnknownSeeder)
}

func TestRegistry_UpdateExternalAddr_SetsAddr(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	id := ids.IdentityID{0x55}
	r.Register(SeederRecord{IdentityID: id})

	addr := netip.MustParseAddrPort("198.51.100.4:51820")
	require.NoError(t, r.UpdateExternalAddr(id, addr))

	got, _ := r.Get(id)
	assert.Equal(t, addr, got.NetCoords.ExternalAddr)
}

func TestRegistry_UpdateExternalAddr_PreservesLocalCandidates(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)

	id := ids.IdentityID{0x56}
	local := []netip.AddrPort{netip.MustParseAddrPort("10.0.0.1:51820")}
	r.Register(SeederRecord{
		IdentityID: id,
		NetCoords:  NetCoords{LocalCandidates: local},
	})

	require.NoError(t, r.UpdateExternalAddr(id, netip.MustParseAddrPort("203.0.113.9:443")))

	got, _ := r.Get(id)
	assert.Equal(t, local, got.NetCoords.LocalCandidates)
}

func TestRegistry_UpdateExternalAddr_UnknownReturnsErr(t *testing.T) {
	r, err := New(DefaultShardCount)
	require.NoError(t, err)
	err = r.UpdateExternalAddr(ids.IdentityID{0xDD}, netip.MustParseAddrPort("1.1.1.1:80"))
	assert.ErrorIs(t, err, ErrUnknownSeeder)
}

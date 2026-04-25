package registry

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/token-bay/token-bay/shared/ids"
)

func TestShard_NewIsEmpty(t *testing.T) {
	s := newShard()
	_, ok := s.get(ids.IdentityID{0x01})
	assert.False(t, ok)
}

func TestShard_PutThenGet_ReturnsCopy(t *testing.T) {
	s := newShard()
	now := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	rec := SeederRecord{
		IdentityID:    ids.IdentityID{0x01},
		Available:     true,
		LastHeartbeat: now,
		Capabilities: Capabilities{
			Models: []string{"claude-opus-4-7"},
		},
	}
	s.put(rec)

	got, ok := s.get(rec.IdentityID)
	assert.True(t, ok)
	assert.Equal(t, rec.IdentityID, got.IdentityID)
	assert.Equal(t, rec.Available, got.Available)
	assert.Equal(t, rec.LastHeartbeat, got.LastHeartbeat)

	// Mutate the returned copy; ensure the store is untouched.
	got.Available = false
	got.Capabilities.Models[0] = "MUTATED"
	got2, ok := s.get(rec.IdentityID)
	assert.True(t, ok)
	assert.True(t, got2.Available, "store should be insulated from mutations on returned copy")
	assert.Equal(t, "claude-opus-4-7", got2.Capabilities.Models[0],
		"models slice should be defensively copied on read")
}

func TestShard_PutOverwrites(t *testing.T) {
	s := newShard()
	id := ids.IdentityID{0x07}
	s.put(SeederRecord{IdentityID: id, Load: 1})
	s.put(SeederRecord{IdentityID: id, Load: 2})

	got, ok := s.get(id)
	assert.True(t, ok)
	assert.Equal(t, 2, got.Load)
}

func TestShard_DeleteRemoves(t *testing.T) {
	s := newShard()
	id := ids.IdentityID{0x07}
	s.put(SeederRecord{IdentityID: id})
	s.delete(id)
	_, ok := s.get(id)
	assert.False(t, ok)
}

func TestShard_DeleteMissing_NoOp(t *testing.T) {
	s := newShard()
	assert.NotPanics(t, func() {
		s.delete(ids.IdentityID{0xFF})
	})
}

func TestShard_Update_AppliesMutation(t *testing.T) {
	s := newShard()
	id := ids.IdentityID{0x09}
	s.put(SeederRecord{IdentityID: id, Load: 0})

	err := s.update(id, func(r *SeederRecord) error {
		r.Load = 5
		return nil
	})
	assert.NoError(t, err)

	got, _ := s.get(id)
	assert.Equal(t, 5, got.Load)
}

func TestShard_Update_UnknownReturnsErr(t *testing.T) {
	s := newShard()
	err := s.update(ids.IdentityID{0xAA}, func(r *SeederRecord) error {
		t.Fatal("update fn should not be called for unknown seeder")
		return nil
	})
	assert.ErrorIs(t, err, ErrUnknownSeeder)
}

func TestShard_Update_PropagatesFnError(t *testing.T) {
	s := newShard()
	id := ids.IdentityID{0x10}
	s.put(SeederRecord{IdentityID: id, Load: 0})

	sentinel := assertErr("boom")
	err := s.update(id, func(r *SeederRecord) error {
		r.Load = 99
		return sentinel
	})
	assert.ErrorIs(t, err, sentinel)

	// On error, the mutation must NOT be persisted (rollback semantics).
	got, _ := s.get(id)
	assert.Equal(t, 0, got.Load, "mutation should be discarded when update fn returns error")
}

func TestShard_Snapshot_ReturnsAllRecords(t *testing.T) {
	s := newShard()
	s.put(SeederRecord{IdentityID: ids.IdentityID{0x01}, Load: 1})
	s.put(SeederRecord{IdentityID: ids.IdentityID{0x02}, Load: 2})

	out := s.snapshot()
	assert.Len(t, out, 2)
}

func TestShard_SweepStale_RemovesAndCounts(t *testing.T) {
	s := newShard()
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour)
	fresh := now.Add(-1 * time.Minute)

	s.put(SeederRecord{IdentityID: ids.IdentityID{0x01}, LastHeartbeat: old})
	s.put(SeederRecord{IdentityID: ids.IdentityID{0x02}, LastHeartbeat: fresh})

	removed := s.sweepStale(now.Add(-1 * time.Hour))
	assert.Equal(t, 1, removed)

	_, ok1 := s.get(ids.IdentityID{0x01})
	_, ok2 := s.get(ids.IdentityID{0x02})
	assert.False(t, ok1)
	assert.True(t, ok2)
}

// assertErr is a tiny helper so the test file can compose simple sentinel
// errors without pulling in another package.
type assertErr string

func (e assertErr) Error() string { return string(e) }

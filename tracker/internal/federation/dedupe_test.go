package federation_test

import (
	"testing"
	"time"

	"github.com/token-bay/token-bay/tracker/internal/federation"
)

func TestDedupe_MarkAndSeen(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Unix(1000, 0))
	d := federation.NewDedupe(time.Minute, 1024, clock.Now)

	id := [32]byte{1}
	if d.Seen(id) {
		t.Fatal("Seen returned true before Mark")
	}
	d.Mark(id)
	if !d.Seen(id) {
		t.Fatal("Seen returned false after Mark")
	}
}

func TestDedupe_TTLExpiry(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Unix(1000, 0))
	d := federation.NewDedupe(time.Minute, 1024, clock.Now)
	id := [32]byte{2}
	d.Mark(id)
	clock.Advance(2 * time.Minute)
	if d.Seen(id) {
		t.Fatal("Seen returned true after TTL")
	}
}

func TestDedupe_CapacityEvicts(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Unix(1000, 0))
	d := federation.NewDedupe(time.Hour, 4, clock.Now)
	for i := byte(0); i < 6; i++ {
		d.Mark([32]byte{i})
		clock.Advance(time.Second) // strictly increasing timestamps
	}
	// Oldest (i=0,1) must have been evicted to keep capacity at 4.
	if d.Seen([32]byte{0}) || d.Seen([32]byte{1}) {
		t.Fatal("expected oldest entries evicted")
	}
	for i := byte(2); i < 6; i++ {
		if !d.Seen([32]byte{i}) {
			t.Fatalf("entry %d unexpectedly evicted", i)
		}
	}
}

func TestDedupe_Seen_ReturnsFalseForStrandedExpiredEntry(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Unix(1000, 0))
	d := federation.NewDedupe(time.Minute, 1024, clock.Now)

	// Mark A, then B. List order (front→back): B, A.
	d.Mark([32]byte{0xa})
	clock.Advance(time.Second)
	d.Mark([32]byte{0xb})

	// Re-mark A — moves to front, refreshes its TTL.
	// List order is now (front→back): A (fresh), B (older).
	clock.Advance(time.Second)
	d.Mark([32]byte{0xa})

	// Wait long enough that B has expired but A has not, then re-mark A
	// again to keep its TTL fresh.
	clock.Advance(45 * time.Second)
	d.Mark([32]byte{0xa})

	// Now wait until B is well past TTL but A is still fresh (because we
	// keep re-marking it). After this point B is "stranded": expired but
	// sitting in the list because evictExpiredLocked walks back→front
	// and may stop early once a non-expired entry is at the back.
	clock.Advance(45 * time.Second)
	if d.Seen([32]byte{0xb}) {
		t.Fatal("Seen returned true for stranded expired entry B")
	}
}

func TestDedupe_Mark_ReMarkRefreshesTTL(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Unix(1000, 0))
	d := federation.NewDedupe(time.Minute, 1024, clock.Now)
	id := [32]byte{0xc}

	d.Mark(id)
	clock.Advance(45 * time.Second)
	d.Mark(id)                      // refresh
	clock.Advance(45 * time.Second) // 90s since first mark, but only 45s since refresh
	if !d.Seen(id) {
		t.Fatal("Seen returned false after re-Mark refreshed TTL")
	}
}

// fakeClock — file-level test helper.
type fakeClock struct{ now time.Time }

func newFakeClock(t time.Time) *fakeClock    { return &fakeClock{now: t} }
func (f *fakeClock) Now() time.Time          { return f.now }
func (f *fakeClock) Advance(d time.Duration) { f.now = f.now.Add(d) }

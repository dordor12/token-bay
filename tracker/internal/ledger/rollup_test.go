package ledger

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

// TestMerkleHash_GoldenVector pins the binary-Merkle-over-entry-hashes
// algorithm. The shape is:
//   - 0 leaves: nil (the orchestrator skips empty hours instead of
//     persisting a sentinel root)
//   - 1 leaf:  leaf itself
//   - N leaves: pair adjacent; duplicate the last leaf when N is odd;
//     SHA-256(left || right) and recurse upward
//
// Federation accepts merkle_root bytes as opaque, but downstream
// equivocation detection compares roots byte-for-byte across peers — so
// the algorithm needs a single canonical form. Changing it later is a
// breaking change to the gossip protocol.
func TestMerkleHash_Empty_ReturnsNil(t *testing.T) {
	if got := merkleHash(nil); got != nil {
		t.Fatalf("empty leaves: got %x, want nil", got)
	}
	if got := merkleHash([][]byte{}); got != nil {
		t.Fatalf("zero leaves: got %x, want nil", got)
	}
}

func TestMerkleHash_SingleLeaf_RootEqualsLeaf(t *testing.T) {
	leaf := bytes.Repeat([]byte{0x01}, 32)
	got := merkleHash([][]byte{leaf})
	if !bytes.Equal(got, leaf) {
		t.Fatalf("single-leaf root: got %x, want %x", got, leaf)
	}
}

func TestMerkleHash_TwoLeaves(t *testing.T) {
	a := bytes.Repeat([]byte{0x01}, 32)
	b := bytes.Repeat([]byte{0x02}, 32)
	want := sha256Concat(a, b)
	got := merkleHash([][]byte{a, b})
	if !bytes.Equal(got, want) {
		t.Fatalf("two-leaf root: got %x, want %x", got, want)
	}
}

func TestMerkleHash_ThreeLeaves_DuplicatesLast(t *testing.T) {
	a := bytes.Repeat([]byte{0x01}, 32)
	b := bytes.Repeat([]byte{0x02}, 32)
	c := bytes.Repeat([]byte{0x03}, 32)

	ab := sha256Concat(a, b)
	cc := sha256Concat(c, c) // odd → duplicate
	want := sha256Concat(ab, cc)

	got := merkleHash([][]byte{a, b, c})
	if !bytes.Equal(got, want) {
		t.Fatalf("three-leaf root: got %x, want %x", got, want)
	}
}

func TestMerkleHash_FourLeaves(t *testing.T) {
	a := bytes.Repeat([]byte{0x01}, 32)
	b := bytes.Repeat([]byte{0x02}, 32)
	c := bytes.Repeat([]byte{0x03}, 32)
	d := bytes.Repeat([]byte{0x04}, 32)

	ab := sha256Concat(a, b)
	cd := sha256Concat(c, d)
	want := sha256Concat(ab, cd)

	got := merkleHash([][]byte{a, b, c, d})
	if !bytes.Equal(got, want) {
		t.Fatalf("four-leaf root: got %x, want %x", got, want)
	}
}

func TestMerkleHash_FiveLeaves_OddCascadesUpward(t *testing.T) {
	// 5 leaves → level1 has 3 nodes (AB, CD, EE) → level2 has 2 nodes
	// (AB+CD, EE+EE) → root.
	a := bytes.Repeat([]byte{0x0a}, 32)
	b := bytes.Repeat([]byte{0x0b}, 32)
	c := bytes.Repeat([]byte{0x0c}, 32)
	d := bytes.Repeat([]byte{0x0d}, 32)
	e := bytes.Repeat([]byte{0x0e}, 32)

	ab := sha256Concat(a, b)
	cd := sha256Concat(c, d)
	ee := sha256Concat(e, e)

	abcd := sha256Concat(ab, cd)
	eeee := sha256Concat(ee, ee)

	want := sha256Concat(abcd, eeee)
	got := merkleHash([][]byte{a, b, c, d, e})
	if !bytes.Equal(got, want) {
		t.Fatalf("five-leaf root: got %x, want %x", got, want)
	}
}

func sha256Concat(parts ...[]byte) []byte {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// --- RunRollupOnce ---

// hourTime returns a time.Time that lands inside the given hour bucket,
// offset by `offsetSec` seconds from the hour's start.
func hourTime(hour uint64, offsetSec int64) time.Time {
	return time.Unix(int64(hour)*3600+offsetSec, 0)
}

// mutableClock wraps an atomic time pointer so tests can advance time
// before each entry append. The fixedClock helper in helpers_test.go is
// fine for single-timestamp tests; the rollup tests need to walk the
// clock across hour boundaries.
type mutableClock struct {
	t atomic.Pointer[time.Time]
}

func newMutableClock(t time.Time) *mutableClock {
	c := &mutableClock{}
	c.t.Store(&t)
	return c
}

func (c *mutableClock) Now() time.Time         { return *c.t.Load() }
func (c *mutableClock) Set(t time.Time)        { c.t.Store(&t) }
func (c *mutableClock) Func() func() time.Time { return c.Now }

// rollupLedgerWithStore opens a Ledger backed by a fresh tempdir Store.
// Returns the ledger, its store, and the SQLite path so restart tests
// can reopen the same DB. The Store is closed via t.Cleanup.
func rollupLedgerWithStore(t *testing.T, clock *mutableClock) (*Ledger, *storage.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.db")
	s, err := storage.Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	_, priv := trackerKeypair()
	l, err := Open(s, priv, WithClock(clock.Func()))
	require.NoError(t, err)
	return l, s, path
}

func TestRunRollupOnce_EmptyHour_DoesNotPersist(t *testing.T) {
	ctx := context.Background()
	clock := newMutableClock(hourTime(100, 0))
	l, s, _ := rollupLedgerWithStore(t, clock)

	require.NoError(t, l.RunRollupOnce(ctx, 50))

	_, _, ok, err := s.GetMerkleRoot(ctx, 50)
	require.NoError(t, err)
	assert.False(t, ok, "empty hour must not persist a sentinel root")
}

func TestRunRollupOnce_NonEmptyHour_PersistsRootAndValidSig(t *testing.T) {
	ctx := context.Background()
	const hour uint64 = 100

	clock := newMutableClock(hourTime(hour, 60))
	l, s, _ := rollupLedgerWithStore(t, clock)

	// Two starter-grant entries timestamped inside hour 100.
	identityA := bytes.Repeat([]byte{0xA1}, 32)
	identityB := bytes.Repeat([]byte{0xB2}, 32)

	_, err := l.IssueStarterGrant(ctx, identityA, 1000)
	require.NoError(t, err)
	clock.Set(hourTime(hour, 120))
	_, err = l.IssueStarterGrant(ctx, identityB, 2000)
	require.NoError(t, err)

	// Move clock to next hour so prevHour=100 is the just-completed hour.
	clock.Set(hourTime(hour+1, 10))

	require.NoError(t, l.RunRollupOnce(ctx, hour))

	root, sig, ok, err := s.GetMerkleRoot(ctx, hour)
	require.NoError(t, err)
	require.True(t, ok, "expected persisted root for hour %d", hour)
	require.Len(t, root, 32, "root must be 32 bytes (federation RootLen)")
	require.Len(t, sig, ed25519.SignatureSize)

	// Root must equal merkleHash(leaves) — drive the expectation from
	// the storage leaves to lock in the algorithm end-to-end.
	leaves, err := s.LeavesForHour(ctx, hour)
	require.NoError(t, err)
	require.Len(t, leaves, 2)
	assert.Equal(t, merkleHash(leaves), root)

	// Signature verifies under the tracker's own pubkey over the
	// canonical preimage.
	trackerPub, _ := trackerKeypair()
	preimage := merkleRootSignPreimage(hour, root)
	assert.True(t, ed25519.Verify(trackerPub, preimage, sig),
		"tracker sig must verify over canonical (hour, root) preimage")
}

func TestRunRollupOnce_AlreadyPersisted_NoOp(t *testing.T) {
	ctx := context.Background()
	const hour uint64 = 100

	clock := newMutableClock(hourTime(hour, 30))
	l, s, _ := rollupLedgerWithStore(t, clock)

	identity := bytes.Repeat([]byte{0xC3}, 32)
	_, err := l.IssueStarterGrant(ctx, identity, 1000)
	require.NoError(t, err)
	clock.Set(hourTime(hour+1, 30))

	require.NoError(t, l.RunRollupOnce(ctx, hour))
	firstRoot, firstSig, ok, err := s.GetMerkleRoot(ctx, hour)
	require.NoError(t, err)
	require.True(t, ok)

	// Second call on the same hour must be a no-op — same row preserved,
	// no error surfaced from ErrDuplicateMerkleRoot.
	require.NoError(t, l.RunRollupOnce(ctx, hour))
	secondRoot, secondSig, _, err := s.GetMerkleRoot(ctx, hour)
	require.NoError(t, err)
	assert.Equal(t, firstRoot, secondRoot)
	assert.Equal(t, firstSig, secondSig)
}

func TestRunRollupOnce_TwoHours_BothPersist(t *testing.T) {
	ctx := context.Background()
	const hourA, hourB uint64 = 100, 101

	clock := newMutableClock(hourTime(hourA, 60))
	l, s, _ := rollupLedgerWithStore(t, clock)

	idA := bytes.Repeat([]byte{0xA1}, 32)
	idB := bytes.Repeat([]byte{0xB2}, 32)

	_, err := l.IssueStarterGrant(ctx, idA, 1000)
	require.NoError(t, err)
	clock.Set(hourTime(hourB, 120))
	_, err = l.IssueStarterGrant(ctx, idB, 2000)
	require.NoError(t, err)

	clock.Set(hourTime(hourB+1, 10))
	require.NoError(t, l.RunRollupOnce(ctx, hourA))
	require.NoError(t, l.RunRollupOnce(ctx, hourB))

	rootA, _, okA, err := s.GetMerkleRoot(ctx, hourA)
	require.NoError(t, err)
	require.True(t, okA)
	rootB, _, okB, err := s.GetMerkleRoot(ctx, hourB)
	require.NoError(t, err)
	require.True(t, okB)
	assert.NotEqual(t, rootA, rootB, "different leaves must produce different roots")

	leavesA, _ := s.LeavesForHour(ctx, hourA)
	leavesB, _ := s.LeavesForHour(ctx, hourB)
	require.Len(t, leavesA, 1)
	require.Len(t, leavesB, 1)
	// Single-leaf root equals leaf hash.
	assert.Equal(t, leavesA[0], rootA)
	assert.Equal(t, leavesB[0], rootB)
}

// TestRunRollupOnce_RestartIdempotent simulates a tracker restart: open
// ledger A, append entries, rollup, close. Reopen a NEW ledger over the
// same DB path; rollup the same hour again. The second rollup must be a
// no-op and must not corrupt the original row.
func TestRunRollupOnce_RestartIdempotent(t *testing.T) {
	ctx := context.Background()
	const hour uint64 = 200

	clockA := newMutableClock(hourTime(hour, 30))
	pathDir := t.TempDir()
	dbPath := filepath.Join(pathDir, "ledger.db")

	storeA, err := storage.Open(ctx, dbPath)
	require.NoError(t, err)
	_, priv := trackerKeypair()
	lA, err := Open(storeA, priv, WithClock(clockA.Func()))
	require.NoError(t, err)

	identity := bytes.Repeat([]byte{0xD4}, 32)
	_, err = lA.IssueStarterGrant(ctx, identity, 1000)
	require.NoError(t, err)

	clockA.Set(hourTime(hour+1, 30))
	require.NoError(t, lA.RunRollupOnce(ctx, hour))
	require.NoError(t, lA.Close())
	require.NoError(t, storeA.Close())

	rootBefore, sigBefore, _, err := mustReadRoot(ctx, t, dbPath, hour)
	require.NoError(t, err)
	require.NotEmpty(t, rootBefore)

	// Reopen and call RunRollupOnce again.
	storeB, err := storage.Open(ctx, dbPath)
	require.NoError(t, err)
	defer storeB.Close()
	clockB := newMutableClock(hourTime(hour+2, 0))
	lB, err := Open(storeB, priv, WithClock(clockB.Func()))
	require.NoError(t, err)
	defer lB.Close()

	require.NoError(t, lB.RunRollupOnce(ctx, hour))

	rootAfter, sigAfter, ok, err := storeB.GetMerkleRoot(ctx, hour)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rootBefore, rootAfter, "restart must not change the persisted root")
	assert.Equal(t, sigBefore, sigAfter, "restart must not change the persisted sig")
}

// mustReadRoot opens a fresh handle on dbPath, reads (root, sig), closes,
// and returns. Used by the restart test to confirm pre-restart state.
func mustReadRoot(ctx context.Context, t *testing.T, dbPath string, hour uint64) ([]byte, []byte, bool, error) {
	t.Helper()
	s, err := storage.Open(ctx, dbPath)
	if err != nil {
		return nil, nil, false, err
	}
	defer s.Close()
	return s.GetMerkleRoot(ctx, hour)
}

// TestStartRollup_TickerPersistsRootForPrevHour drives the goroutine
// end-to-end at a 5 ms cadence. The Eventually budget is loose to keep
// the test stable on slow CI, but the path under test is the same one
// production exercises: ticker → derive prevHour from nowFn → RunRollupOnce.
func TestStartRollup_TickerPersistsRootForPrevHour(t *testing.T) {
	ctx := context.Background()
	const hour uint64 = 400

	clock := newMutableClock(hourTime(hour, 60))
	l, s, _ := rollupLedgerWithStore(t, clock)

	identity := bytes.Repeat([]byte{0xF6}, 32)
	_, err := l.IssueStarterGrant(ctx, identity, 1000)
	require.NoError(t, err)

	// Advance clock so prevHour computed inside the goroutine == hour.
	clock.Set(hourTime(hour+1, 0))

	l.StartRollup(5 * time.Millisecond)
	t.Cleanup(func() { _ = l.Close() })

	require.Eventually(t, func() bool {
		_, _, ok, err := s.GetMerkleRoot(ctx, hour)
		return err == nil && ok
	}, 2*time.Second, 5*time.Millisecond, "rollup goroutine did not persist root for hour=%d", hour)
}

// TestClose_StopsRollupGoroutine confirms Close drains the goroutine
// (wg.Wait returns) and is idempotent on a second call.
func TestClose_StopsRollupGoroutine(t *testing.T) {
	clock := newMutableClock(hourTime(100, 0))
	l, _, _ := rollupLedgerWithStore(t, clock)

	l.StartRollup(5 * time.Millisecond)

	// First Close stops the loop and waits.
	done := make(chan struct{})
	go func() {
		_ = l.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return within 2s — goroutine likely leaked")
	}

	// Second Close is a no-op.
	require.NoError(t, l.Close())
}

// TestRunRollupOnce_ConcurrentCallsForSameHour confirms two parallel
// rollups for the same hour finish without race-detector complaints and
// produce exactly one persisted row (the second call observes the
// already-persisted root, or loses the storage write race and treats
// ErrDuplicateMerkleRoot as a no-op).
func TestRunRollupOnce_ConcurrentCallsForSameHour(t *testing.T) {
	ctx := context.Background()
	const hour uint64 = 300

	clock := newMutableClock(hourTime(hour, 60))
	l, s, _ := rollupLedgerWithStore(t, clock)

	identity := bytes.Repeat([]byte{0xE5}, 32)
	_, err := l.IssueStarterGrant(ctx, identity, 1000)
	require.NoError(t, err)
	clock.Set(hourTime(hour+1, 0))

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- l.RunRollupOnce(ctx, hour)
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e, "concurrent rollup must not surface ErrDuplicateMerkleRoot")
	}

	_, _, ok, err := s.GetMerkleRoot(ctx, hour)
	require.NoError(t, err)
	require.True(t, ok)
}

package ccbridge

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeChecker is a minimal ActiveClientChecker for tests: a set
// of "active" public keys, looked up by ClientHash.
type fakeChecker struct {
	active map[string]bool // keyed by ClientHash
}

func (f *fakeChecker) IsActive(pub ed25519.PublicKey) bool {
	if f.active == nil {
		return false
	}
	return f.active[ClientHash(pub)]
}

func (f *fakeChecker) IsActiveByHash(hash string) bool {
	if f.active == nil {
		return false
	}
	return f.active[hash]
}

func TestActiveClientChecker_HashLookup(t *testing.T) {
	keyA := make(ed25519.PublicKey, ed25519.PublicKeySize)
	keyA[0] = 0x01
	keyB := make(ed25519.PublicKey, ed25519.PublicKeySize)
	keyB[0] = 0x02

	c := &fakeChecker{active: map[string]bool{ClientHash(keyA): true}}
	assert.True(t, c.IsActive(keyA))
	assert.False(t, c.IsActive(keyB))
}

func TestJanitorScan_RemovesInactiveAndStale(t *testing.T) {
	root := t.TempDir()

	keyActive := make(ed25519.PublicKey, ed25519.PublicKeySize)
	keyActive[0] = 0x01
	keyInactive := make(ed25519.PublicKey, ed25519.PublicKeySize)
	keyInactive[0] = 0x02

	activeDir := filepath.Join(root, ClientHash(keyActive))
	inactiveDir := filepath.Join(root, ClientHash(keyInactive))
	require.NoError(t, os.MkdirAll(activeDir, 0o700))
	require.NoError(t, os.MkdirAll(inactiveDir, 0o700))

	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(activeDir, old, old))
	require.NoError(t, os.Chtimes(inactiveDir, old, old))

	checker := &fakeChecker{active: map[string]bool{ClientHash(keyActive): true}}
	j := &Janitor{Root: root, Checker: checker, Grace: time.Minute}

	removed, err := j.Scan()
	require.NoError(t, err)
	assert.Equal(t, []string{ClientHash(keyInactive)}, removed)

	_, err = os.Stat(activeDir)
	assert.NoError(t, err)
	_, err = os.Stat(inactiveDir)
	assert.True(t, os.IsNotExist(err), "inactive dir should be gone")
}

func TestJanitorScan_RespectsGracePeriod(t *testing.T) {
	root := t.TempDir()
	keyInactive := make(ed25519.PublicKey, ed25519.PublicKeySize)
	keyInactive[0] = 0x33
	dir := filepath.Join(root, ClientHash(keyInactive))
	require.NoError(t, os.MkdirAll(dir, 0o700))

	checker := &fakeChecker{} // nothing active
	j := &Janitor{Root: root, Checker: checker, Grace: time.Hour}

	removed, err := j.Scan()
	require.NoError(t, err)
	assert.Empty(t, removed, "fresh folder must survive grace window")
	_, err = os.Stat(dir)
	assert.NoError(t, err)
}

func TestJanitorScan_IgnoresUnknownEntries(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "stray.txt"), []byte{}, 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "not-a-hash"), 0o700))

	j := &Janitor{Root: root, Checker: &fakeChecker{}, Grace: time.Minute}
	removed, err := j.Scan()
	require.NoError(t, err)
	assert.Empty(t, removed)
}

func TestJanitorRun_TicksAndStopsOnContextCancel(t *testing.T) {
	root := t.TempDir()
	keyInactive := make(ed25519.PublicKey, ed25519.PublicKeySize)
	keyInactive[0] = 0x77
	dir := filepath.Join(root, ClientHash(keyInactive))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(dir, old, old))

	j := &Janitor{
		Root:     root,
		Checker:  &fakeChecker{},
		Grace:    time.Minute,
		Interval: 20 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() { j.Run(ctx); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("janitor Run didn't return after context cancel")
	}

	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err))
}

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDiscoveryFile_AtomicWriteAndRead confirms writeDiscoveryFile uses the
// temp-file + rename pattern so a concurrent reader (the hooks subprocess)
// never sees a half-written URL. The contract — atomic write + simple
// readback — is the single integration seam between the long-lived sidecar
// and per-event hook subprocesses, so the round-trip is asserted directly.
func TestDiscoveryFile_AtomicWriteAndRead(t *testing.T) {
	dir := t.TempDir()
	url := "http://127.0.0.1:53421/"
	require.NoError(t, writeDiscoveryFile(dir, url))

	got, err := readDiscoveryFile(dir)
	require.NoError(t, err)
	assert.Equal(t, url, got)

	// No leftover tmp files in the dir.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		assert.False(t, isTempArtifact(e.Name()), "leftover tmp file %s", e.Name())
	}
}

// TestDiscoveryFile_MissingReturnsErrNotExist gives the hook subprocess a
// clean signal to exit 0 silently — a missing sidecar must never block the
// host Claude Code turn.
func TestDiscoveryFile_MissingReturnsErrNotExist(t *testing.T) {
	dir := t.TempDir()
	_, err := readDiscoveryFile(dir)
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// TestDiscoveryFile_RemoveIsIdempotent — graceful shutdown calls remove,
// crashes leave the file (intentional: the next sidecar Start overwrites
// it atomically). Idempotence keeps the cmd layer simple.
func TestDiscoveryFile_RemoveIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeDiscoveryFile(dir, "http://127.0.0.1:1/"))
	require.NoError(t, removeDiscoveryFile(dir))
	// Second remove must not error.
	require.NoError(t, removeDiscoveryFile(dir))
}

// TestDiscoveryFile_PathIsUnderCfgDir documents the on-disk layout — the
// hook subprocess constructs the same path from --config's directory.
func TestDiscoveryFile_PathIsUnderCfgDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeDiscoveryFile(dir, "http://127.0.0.1:1/"))
	_, err := os.Stat(filepath.Join(dir, discoveryFilename))
	require.NoError(t, err)
}

// isTempArtifact matches the temp-file naming convention used by atomic
// writers under the discovery dir. Keeping the matcher local to the test
// (rather than exposing it from the production code) keeps the surface
// area small.
func isTempArtifact(name string) bool {
	return len(name) > len(discoveryFilename) &&
		name[:len(discoveryFilename)] == discoveryFilename &&
		name != discoveryFilename
}

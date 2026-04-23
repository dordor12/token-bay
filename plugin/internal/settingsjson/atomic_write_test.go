package settingsjson

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAtomicWriteFile_CreatesFileWithContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	content := []byte(`{"key":"value"}`)

	err := atomicWriteFile(path, content, 0o600)

	require.NoError(t, err)
	read, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, content, read)
}

func TestAtomicWriteFile_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"old":true}`), 0o600))

	err := atomicWriteFile(path, []byte(`{"new":true}`), 0o600)

	require.NoError(t, err)
	read, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, []byte(`{"new":true}`), read)
}

func TestAtomicWriteFile_LeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")

	err := atomicWriteFile(path, []byte(`{}`), 0o600)
	require.NoError(t, err)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	assert.Equal(t, "out.json", entries[0].Name())
}

func TestResolveSymlink_PlainFile_ReturnsSamePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.json")
	require.NoError(t, os.WriteFile(path, []byte(`{}`), 0o600))

	resolved, err := resolveSymlinkTarget(path, dir)
	require.NoError(t, err)
	assert.Equal(t, path, resolved)
}

func TestResolveSymlink_SymlinkWithinBase_ReturnsTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.json")
	link := filepath.Join(dir, "link.json")
	require.NoError(t, os.WriteFile(target, []byte(`{}`), 0o600))
	require.NoError(t, os.Symlink(target, link))

	resolved, err := resolveSymlinkTarget(link, dir)
	require.NoError(t, err)
	assert.Equal(t, target, resolved)
}

func TestResolveSymlink_SymlinkEscapesBase_ReturnsError(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	outsideTarget := filepath.Join(outside, "escape.json")
	require.NoError(t, os.WriteFile(outsideTarget, []byte(`{}`), 0o600))
	link := filepath.Join(base, "link.json")
	require.NoError(t, os.Symlink(outsideTarget, link))

	_, err := resolveSymlinkTarget(link, base)
	assert.ErrorIs(t, err, ErrSymlinkEscapesClaudeDir)
}

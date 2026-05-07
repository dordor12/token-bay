package identity

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadKey_NotFound(t *testing.T) {
	_, err := LoadKey(filepath.Join(t.TempDir(), "missing"))
	require.ErrorIs(t, err, ErrKeyNotFound)
}

func TestLoadKey_BadSize(t *testing.T) {
	p := filepath.Join(t.TempDir(), "identity.key")
	require.NoError(t, os.WriteFile(p, []byte("too short"), 0o600))
	_, err := LoadKey(p)
	require.ErrorIs(t, err, ErrInvalidKey)
}

func TestSaveKey_RoundTripPreservesPrivateKey(t *testing.T) {
	s, err := Generate()
	require.NoError(t, err)

	p := filepath.Join(t.TempDir(), "identity.key")
	require.NoError(t, SaveKey(p, s))

	s2, err := LoadKey(p)
	require.NoError(t, err)
	assert.Equal(t, []byte(s.PrivateKey()), []byte(s2.PrivateKey()))
	assert.Equal(t, s.IdentityID(), s2.IdentityID())
}

func TestSaveKey_RefusesOverwrite(t *testing.T) {
	s, err := Generate()
	require.NoError(t, err)
	p := filepath.Join(t.TempDir(), "identity.key")
	require.NoError(t, SaveKey(p, s))

	err = SaveKey(p, s)
	require.True(t, errors.Is(err, ErrKeyExists), "got %v", err)
}

func TestSaveKey_FileMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not honor Unix file modes")
	}
	s, err := Generate()
	require.NoError(t, err)
	p := filepath.Join(t.TempDir(), "identity.key")
	require.NoError(t, SaveKey(p, s))

	info, err := os.Stat(p)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestSaveKey_FileSizeIs32Bytes(t *testing.T) {
	s, err := Generate()
	require.NoError(t, err)
	p := filepath.Join(t.TempDir(), "identity.key")
	require.NoError(t, SaveKey(p, s))
	info, err := os.Stat(p)
	require.NoError(t, err)
	assert.EqualValues(t, 32, info.Size())
}

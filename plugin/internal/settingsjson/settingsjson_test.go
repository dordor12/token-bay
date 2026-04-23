package settingsjson

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewStoreAt_StoresProvidedPaths(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	rollbackPath := filepath.Join(dir, "rollback.json")

	store := NewStoreAt(settingsPath, rollbackPath)

	require.NotNil(t, store)
	assert.Equal(t, settingsPath, store.SettingsPath)
	assert.Equal(t, rollbackPath, store.RollbackPath)
}

func TestNewStore_ResolvesDefaultPaths(t *testing.T) {
	store, err := NewStore()

	require.NoError(t, err)
	require.NotNil(t, store)
	assert.Contains(t, store.SettingsPath, ".claude")
	assert.Contains(t, store.SettingsPath, "settings.json")
	assert.Contains(t, store.RollbackPath, ".token-bay")
	assert.Contains(t, store.RollbackPath, "settings-rollback.json")
}

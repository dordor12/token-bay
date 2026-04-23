package settingsjson

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetState_MissingSettingsFile_ReturnsFileDoesNotExist(t *testing.T) {
	dir := t.TempDir()
	store := NewStoreAt(filepath.Join(dir, "settings.json"), filepath.Join(dir, "rollback.json"))

	state, err := store.GetState("http://127.0.0.1:53421")
	require.NoError(t, err)
	assert.False(t, state.SettingsFileExists)
}

func TestGetState_EmptyEnvBlock_ReturnsCleanState(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{"env": {}}`), 0o600))
	store := NewStoreAt(settingsPath, filepath.Join(dir, "rollback.json"))

	state, err := store.GetState("http://127.0.0.1:53421")

	require.NoError(t, err)
	assert.True(t, state.SettingsFileExists)
	assert.False(t, state.HasJSONCComments)
	assert.Empty(t, state.ExistingBaseURL)
	assert.False(t, state.BedrockEnabled)
	assert.False(t, state.VertexEnabled)
	assert.False(t, state.FoundryEnabled)
}

func TestGetState_ExistingNonMatchingRedirect_FlagsIt(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{"env": {"ANTHROPIC_BASE_URL": "https://proxy.example.com"}}`), 0o600))
	store := NewStoreAt(settingsPath, filepath.Join(dir, "rollback.json"))

	state, err := store.GetState("http://127.0.0.1:53421")

	require.NoError(t, err)
	assert.Equal(t, "https://proxy.example.com", state.ExistingBaseURL)
	assert.False(t, state.ExistingBaseURLMatches)
}

func TestGetState_MatchingRedirect_FlagsMatch(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{"env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:53421"}}`), 0o600))
	store := NewStoreAt(settingsPath, filepath.Join(dir, "rollback.json"))

	state, err := store.GetState("http://127.0.0.1:53421")

	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:53421", state.ExistingBaseURL)
	assert.True(t, state.ExistingBaseURLMatches)
}

func TestGetState_BedrockEnabled_FlagsIt(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{"env": {"CLAUDE_CODE_USE_BEDROCK": "1"}}`), 0o600))
	store := NewStoreAt(settingsPath, filepath.Join(dir, "rollback.json"))

	state, err := store.GetState("http://127.0.0.1:53421")

	require.NoError(t, err)
	assert.True(t, state.BedrockEnabled)
}

func TestGetState_BedrockFalse_NotFlagged(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{"env": {"CLAUDE_CODE_USE_BEDROCK": "0"}}`), 0o600))
	store := NewStoreAt(settingsPath, filepath.Join(dir, "rollback.json"))

	state, err := store.GetState("http://127.0.0.1:53421")

	require.NoError(t, err)
	assert.False(t, state.BedrockEnabled)
}

func TestGetState_JSONCContent_FlagsIt(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{
  // a comment
  "env": {}
}`), 0o600))
	store := NewStoreAt(settingsPath, filepath.Join(dir, "rollback.json"))

	state, err := store.GetState("http://127.0.0.1:53421")

	require.NoError(t, err)
	assert.True(t, state.HasJSONCComments)
}

func TestGetState_AfterEnterNetworkMode_InNetworkModeIsTrue(t *testing.T) {
	store, _, _ := newTestStore(t)
	require.NoError(t, store.EnterNetworkMode("http://127.0.0.1:53421", "session-x"))

	state, err := store.GetState("http://127.0.0.1:53421")
	require.NoError(t, err)
	assert.True(t, state.InNetworkMode)
}

func TestGetState_WithoutEntering_InNetworkModeIsFalse(t *testing.T) {
	store, _, _ := newTestStore(t)

	state, err := store.GetState("http://127.0.0.1:53421")
	require.NoError(t, err)
	assert.False(t, state.InNetworkMode)
}

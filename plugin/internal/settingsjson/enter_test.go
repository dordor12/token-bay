package settingsjson

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	require.NoError(t, os.MkdirAll(claudeDir, 0o700))
	settings := filepath.Join(claudeDir, "settings.json")
	rollback := filepath.Join(dir, ".token-bay", "settings-rollback.json")
	return NewStoreAt(settings, rollback), settings, rollback
}

func readEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var parsed struct {
		Env map[string]string `json:"env"`
	}
	require.NoError(t, json.Unmarshal(data, &parsed))
	return parsed.Env
}

func TestEnterNetworkMode_MissingSettings_CreatesFile(t *testing.T) {
	store, settings, rollback := newTestStore(t)

	err := store.EnterNetworkMode("http://127.0.0.1:53421", "session-x")

	require.NoError(t, err)
	env := readEnv(t, settings)
	assert.Equal(t, "http://127.0.0.1:53421", env["ANTHROPIC_BASE_URL"])
	_, err = os.Stat(rollback)
	assert.NoError(t, err)
}

func TestEnterNetworkMode_ExistingSettings_PreservesOtherKeys(t *testing.T) {
	store, settings, _ := newTestStore(t)
	original := `{"env":{"OTHER_VAR":"keep-me"},"otherRoot":{"k":"v"}}`
	require.NoError(t, os.WriteFile(settings, []byte(original), 0o600))

	require.NoError(t, store.EnterNetworkMode("http://127.0.0.1:53421", "session-x"))

	env := readEnv(t, settings)
	assert.Equal(t, "http://127.0.0.1:53421", env["ANTHROPIC_BASE_URL"])
	assert.Equal(t, "keep-me", env["OTHER_VAR"])

	raw, err := os.ReadFile(settings)
	require.NoError(t, err)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(raw, &parsed))
	assert.Equal(t, map[string]any{"k": "v"}, parsed["otherRoot"])
}

func TestEnterNetworkMode_RollbackCapturesPriorState(t *testing.T) {
	store, settings, rollback := newTestStore(t)
	require.NoError(t, os.WriteFile(settings, []byte(`{"env":{"OTHER":"x"}}`), 0o600))

	require.NoError(t, store.EnterNetworkMode("http://127.0.0.1:53421", "session-x"))

	j, err := readRollback(rollback)
	require.NoError(t, err)
	assert.True(t, j.PreFallback.SettingsFileExisted)
	assert.False(t, j.PreFallback.BaseURLWasSet)
	assert.Empty(t, j.PreFallback.BaseURLPriorValue)
	assert.Equal(t, "http://127.0.0.1:53421", j.SidecarURL)
	assert.Equal(t, "session-x", j.SessionID)
}

func TestEnterNetworkMode_IdempotentReEnter_Succeeds(t *testing.T) {
	store, settings, _ := newTestStore(t)
	require.NoError(t, os.WriteFile(settings, []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:53421"}}`), 0o600))

	err := store.EnterNetworkMode("http://127.0.0.1:53421", "session-x")

	require.NoError(t, err)
	env := readEnv(t, settings)
	assert.Equal(t, "http://127.0.0.1:53421", env["ANTHROPIC_BASE_URL"])
}

func TestEnterNetworkMode_PreExistingNonMatchingRedirect_Refuses(t *testing.T) {
	store, settings, _ := newTestStore(t)
	require.NoError(t, os.WriteFile(settings, []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://someone-else.example.com"}}`), 0o600))

	err := store.EnterNetworkMode("http://127.0.0.1:53421", "session-x")

	assert.ErrorIs(t, err, ErrPreExistingRedirect)
}

func TestEnterNetworkMode_BedrockEnabled_Refuses(t *testing.T) {
	store, settings, _ := newTestStore(t)
	require.NoError(t, os.WriteFile(settings, []byte(`{"env":{"CLAUDE_CODE_USE_BEDROCK":"1"}}`), 0o600))

	err := store.EnterNetworkMode("http://127.0.0.1:53421", "session-x")
	assert.ErrorIs(t, err, ErrBedrockProvider)
}

func TestEnterNetworkMode_VertexEnabled_Refuses(t *testing.T) {
	store, settings, _ := newTestStore(t)
	require.NoError(t, os.WriteFile(settings, []byte(`{"env":{"CLAUDE_CODE_USE_VERTEX":"true"}}`), 0o600))

	err := store.EnterNetworkMode("http://127.0.0.1:53421", "session-x")
	assert.ErrorIs(t, err, ErrVertexProvider)
}

func TestEnterNetworkMode_JSONCComments_Refuses(t *testing.T) {
	store, settings, _ := newTestStore(t)
	require.NoError(t, os.WriteFile(settings, []byte(`{
  // a comment that would be lost
  "env": {}
}`), 0o600))

	err := store.EnterNetworkMode("http://127.0.0.1:53421", "session-x")
	assert.ErrorIs(t, err, ErrIncompatibleJSONCComments)
}

func TestEnterNetworkMode_ConcurrentCalls_Serialize(t *testing.T) {
	store, _, _ := newTestStore(t)

	done := make(chan error, 2)
	go func() { done <- store.EnterNetworkMode("http://127.0.0.1:53421", "A") }()
	go func() { done <- store.EnterNetworkMode("http://127.0.0.1:53421", "B") }()

	for i := 0; i < 2; i++ {
		err := <-done
		// Both calls should succeed; they're idempotent since we write the
		// same URL. The mutex prevents interleaving.
		assert.Falsef(t, errors.Is(err, ErrPreExistingRedirect),
			"concurrent entry with same URL should be idempotent: got %v", err)
	}
}

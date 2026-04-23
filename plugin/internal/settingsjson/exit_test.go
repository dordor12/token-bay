package settingsjson

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExitNetworkMode_AfterEnter_RemovesRedirectAndJournal(t *testing.T) {
	store, settings, rollback := newTestStore(t)
	require.NoError(t, os.WriteFile(settings, []byte(`{"env":{"OTHER":"keep"}}`), 0o600))
	require.NoError(t, store.EnterNetworkMode("http://127.0.0.1:53421", "s"))

	require.NoError(t, store.ExitNetworkMode())

	env := readEnv(t, settings)
	_, present := env["ANTHROPIC_BASE_URL"]
	assert.False(t, present, "ANTHROPIC_BASE_URL should be removed after exit")
	assert.Equal(t, "keep", env["OTHER"])
	_, err := os.Stat(rollback)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestExitNetworkMode_PriorValueRestored(t *testing.T) {
	store, settings, _ := newTestStore(t)
	require.NoError(t, os.WriteFile(settings, []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:53421"}}`), 0o600))
	require.NoError(t, store.EnterNetworkMode("http://127.0.0.1:53421", "s"))
	// Tamper the rollback to simulate a non-Token-Bay prior value
	// (normally EnterNetworkMode refuses this case, but we want to verify
	// the restore code path itself.)
	j, err := readRollback(store.RollbackPath)
	require.NoError(t, err)
	j.PreFallback.BaseURLWasSet = true
	j.PreFallback.BaseURLPriorValue = "https://user-proxy.example.com"
	require.NoError(t, writeRollback(store.RollbackPath, *j))

	require.NoError(t, store.ExitNetworkMode())

	env := readEnv(t, settings)
	assert.Equal(t, "https://user-proxy.example.com", env["ANTHROPIC_BASE_URL"])
}

func TestExitNetworkMode_FileCreatedByUs_LeavesNonIntrusiveContent(t *testing.T) {
	store, settings, _ := newTestStore(t)
	// No settings.json exists before Enter.
	require.NoError(t, store.EnterNetworkMode("http://127.0.0.1:53421", "s"))

	require.NoError(t, store.ExitNetworkMode())

	// Settings file was created by us with just our key; after exit it
	// should no longer contain our key. The file may still exist with
	// an empty env block — that's acceptable for v1.
	raw, err := os.ReadFile(settings)
	require.NoError(t, err)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(raw, &parsed))
	envAny, hasEnv := parsed["env"]
	if hasEnv {
		env := envAny.(map[string]any)
		_, hasURL := env["ANTHROPIC_BASE_URL"]
		assert.False(t, hasURL)
	}
}

func TestExitNetworkMode_MissingJournal_BestEffortRemovesMatchingKey(t *testing.T) {
	store, settings, rollback := newTestStore(t)
	// Simulate a state where our redirect was written but the journal is
	// lost (e.g., manually deleted or a partial-rollback from a prior crash).
	require.NoError(t, os.WriteFile(settings, []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:53421","OTHER":"x"}}`), 0o600))
	_, err := os.Stat(rollback)
	require.ErrorIs(t, err, os.ErrNotExist)

	require.NoError(t, store.ExitNetworkModeBestEffort("http://127.0.0.1:53421"))

	env := readEnv(t, settings)
	_, present := env["ANTHROPIC_BASE_URL"]
	assert.False(t, present)
	assert.Equal(t, "x", env["OTHER"])
}

func TestExitNetworkMode_BestEffort_LeavesNonMatchingKeyAlone(t *testing.T) {
	store, settings, _ := newTestStore(t)
	require.NoError(t, os.WriteFile(settings, []byte(`{"env":{"ANTHROPIC_BASE_URL":"https://someone-else.example.com"}}`), 0o600))

	require.NoError(t, store.ExitNetworkModeBestEffort("http://127.0.0.1:53421"))

	env := readEnv(t, settings)
	assert.Equal(t, "https://someone-else.example.com", env["ANTHROPIC_BASE_URL"])
}

func TestExitNetworkMode_NoJournal_ReturnsError(t *testing.T) {
	store, _, _ := newTestStore(t)

	err := store.ExitNetworkMode()
	assert.Error(t, err)
}

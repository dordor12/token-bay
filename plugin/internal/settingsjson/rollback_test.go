package settingsjson

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteRollback_CreatesFileWithJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "rollback.json")
	j := RollbackJournal{
		EnteredAt:  1713891200,
		SidecarURL: "http://127.0.0.1:53421",
		SessionID:  "abc-123",
		PreFallback: PreFallback{
			BaseURLWasSet:       true,
			BaseURLPriorValue:   "https://proxy.example.com",
			SettingsFileExisted: true,
		},
	}

	err := writeRollback(path, j)

	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var decoded RollbackJournal
	require.NoError(t, json.Unmarshal(raw, &decoded))
	assert.Equal(t, j, decoded)
}

func TestReadRollback_MissingFile_ReturnsErrNotExist(t *testing.T) {
	dir := t.TempDir()
	_, err := readRollback(filepath.Join(dir, "nope.json"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestReadRollback_ExistingFile_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollback.json")
	j := RollbackJournal{
		EnteredAt:  42,
		SidecarURL: "http://localhost:9000",
		SessionID:  "s1",
		PreFallback: PreFallback{
			BaseURLWasSet:       false,
			BaseURLPriorValue:   "",
			SettingsFileExisted: false,
		},
	}
	require.NoError(t, writeRollback(path, j))

	got, err := readRollback(path)
	require.NoError(t, err)
	assert.Equal(t, &j, got)
}

func TestDeleteRollback_ExistingFile_Deletes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollback.json")
	require.NoError(t, os.WriteFile(path, []byte(`{}`), 0o600))

	require.NoError(t, deleteRollback(path))

	_, err := os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestDeleteRollback_MissingFile_NotAnError(t *testing.T) {
	dir := t.TempDir()
	err := deleteRollback(filepath.Join(dir, "nope.json"))
	assert.NoError(t, err)
}

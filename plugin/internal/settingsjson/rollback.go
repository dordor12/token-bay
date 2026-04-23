package settingsjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// RollbackJournal records the pre-fallback state of settings.json so that
// ExitNetworkMode can restore it precisely. Written alongside the settings
// mutation in EnterNetworkMode; consumed in ExitNetworkMode.
type RollbackJournal struct {
	EnteredAt   int64       `json:"entered_at"`   // unix seconds
	SidecarURL  string      `json:"sidecar_url"`  // the URL we redirected to
	SessionID   string      `json:"session_id"`   // Claude Code session id, if known
	PreFallback PreFallback `json:"pre_fallback"` // what we need to restore
}

// PreFallback captures the settings state prior to entering network mode.
type PreFallback struct {
	BaseURLWasSet       bool   `json:"base_url_was_set"`
	BaseURLPriorValue   string `json:"base_url_prior_value"`
	SettingsFileExisted bool   `json:"settings_file_existed"`
}

// writeRollback marshals j and atomically writes it to path. Creates the
// parent directory (perm 0700) if missing.
func writeRollback(path string, j RollbackJournal) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("settingsjson: mkdir %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return fmt.Errorf("settingsjson: marshal rollback: %w", err)
	}
	return atomicWriteFile(path, data, 0o600)
}

// readRollback returns the journal at path. Returns an os.ErrNotExist-wrapped
// error if the file is absent.
func readRollback(path string) (*RollbackJournal, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var j RollbackJournal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("settingsjson: unmarshal rollback %s: %w", path, err)
	}
	return &j, nil
}

// deleteRollback removes the journal. A missing file is not an error —
// ExitNetworkMode may be called redundantly or after a crash.
func deleteRollback(path string) error {
	err := os.Remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("settingsjson: remove rollback %s: %w", path, err)
}

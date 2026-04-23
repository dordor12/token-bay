// Package settingsjson provides atomic mutation of Claude Code's settings.json
// for Token-Bay mid-session redirect. See plugin spec §2.5, §5.3, §5.5.
//
// This package is pure state management — no network I/O, no file watching,
// no IPC. Callers in internal/sidecar orchestrate the lifecycle.
package settingsjson

import (
	"os"
	"path/filepath"
	"sync"
)

// Store owns paths to the settings file and rollback journal.
// Safe for concurrent use; Enter/Exit are mutually exclusive.
type Store struct {
	SettingsPath string
	RollbackPath string
	mu           sync.Mutex
}

// NewStore constructs a Store using the default paths under the user's
// home directory: ~/.claude/settings.json and ~/.token-bay/settings-rollback.json.
func NewStore() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return NewStoreAt(
		filepath.Join(home, ".claude", "settings.json"),
		filepath.Join(home, ".token-bay", "settings-rollback.json"),
	), nil
}

// NewStoreAt constructs a Store with explicit paths — primarily for tests.
func NewStoreAt(settingsPath, rollbackPath string) *Store {
	return &Store{
		SettingsPath: settingsPath,
		RollbackPath: rollbackPath,
	}
}

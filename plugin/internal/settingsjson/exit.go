package settingsjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ExitNetworkMode reads the rollback journal, restores the pre-fallback
// state of settings.json, and deletes the journal. If the journal is absent,
// returns an error — callers needing best-effort cleanup should use
// ExitNetworkModeBestEffort.
func (s *Store) ExitNetworkMode() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	journal, err := readRollback(s.RollbackPath)
	if err != nil {
		return fmt.Errorf("settingsjson: read rollback: %w", err)
	}

	if err := s.restoreFromJournal(*journal); err != nil {
		return err
	}

	return deleteRollback(s.RollbackPath)
}

// ExitNetworkModeBestEffort clears our ANTHROPIC_BASE_URL key without
// consulting the rollback journal. Used when the journal is missing but
// the caller knows which sidecar URL is active (e.g., crash recovery).
// No prior-value restoration is performed; the key is simply removed.
// If the current value doesn't match sidecarURL, the file is left
// untouched — we don't overwrite someone else's redirect.
func (s *Store) ExitNetworkModeBestEffort(sidecarURL string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	claudeDir := filepath.Dir(s.SettingsPath)

	info, err := os.Lstat(s.SettingsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("settingsjson: lstat %s: %w", s.SettingsPath, err)
	}
	writePath := s.SettingsPath
	if info.Mode()&os.ModeSymlink != 0 {
		writePath, err = resolveSymlinkTarget(s.SettingsPath, claudeDir)
		if err != nil {
			return err
		}
	}

	stripped, present, err := stripBaseURLIfMatches(writePath, sidecarURL)
	if err != nil {
		return err
	}
	if !present {
		return nil
	}
	return atomicWriteFile(writePath, stripped, 0o600)
}

// restoreFromJournal rewrites settings.json according to the journal:
// if BaseURLWasSet, set ANTHROPIC_BASE_URL back to BaseURLPriorValue;
// otherwise remove our key entirely.
func (s *Store) restoreFromJournal(j RollbackJournal) error {
	claudeDir := filepath.Dir(s.SettingsPath)

	writePath := s.SettingsPath
	if info, err := os.Lstat(s.SettingsPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			writePath, err = resolveSymlinkTarget(s.SettingsPath, claudeDir)
			if err != nil {
				return err
			}
		}
	}

	data, err := os.ReadFile(writePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Nothing to restore; the settings file was removed externally.
			return nil
		}
		return fmt.Errorf("settingsjson: read %s: %w", writePath, err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("settingsjson: parse %s: %w", writePath, err)
	}

	envAny, hasEnv := root["env"]
	var env map[string]any
	if hasEnv {
		env, _ = envAny.(map[string]any)
	}
	if env == nil {
		env = map[string]any{}
	}

	if j.PreFallback.BaseURLWasSet {
		env["ANTHROPIC_BASE_URL"] = j.PreFallback.BaseURLPriorValue
	} else {
		delete(env, "ANTHROPIC_BASE_URL")
	}
	root["env"] = env

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("settingsjson: marshal restore: %w", err)
	}
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return atomicWriteFile(writePath, out, 0o600)
}

// stripBaseURLIfMatches reads path, removes env.ANTHROPIC_BASE_URL iff it
// equals sidecarURL, returns the new bytes and whether a mutation was made.
func stripBaseURLIfMatches(path, sidecarURL string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("settingsjson: read %s: %w", path, err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, false, fmt.Errorf("settingsjson: parse %s: %w", path, err)
	}

	envAny, hasEnv := root["env"]
	if !hasEnv {
		return nil, false, nil
	}
	env, ok := envAny.(map[string]any)
	if !ok {
		return nil, false, nil
	}
	current, ok := env["ANTHROPIC_BASE_URL"].(string)
	if !ok || current != sidecarURL {
		return nil, false, nil
	}
	delete(env, "ANTHROPIC_BASE_URL")
	root["env"] = env

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, false, fmt.Errorf("settingsjson: marshal strip: %w", err)
	}
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return out, true, nil
}

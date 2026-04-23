package settingsjson

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// EnterNetworkMode writes settings.json with env.ANTHROPIC_BASE_URL=sidecarURL,
// preserving all other settings, and records a rollback journal. Returns a
// sentinel error if the state is incompatible (see errors.go).
func (s *Store) EnterNetworkMode(sidecarURL, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.GetState(sidecarURL)
	if err != nil {
		return err
	}

	// Incompatibility checks. Order matters: provider checks first because
	// they're stronger "this won't work at all" signals than "we can't
	// preserve comments."
	if state.BedrockEnabled {
		return ErrBedrockProvider
	}
	if state.VertexEnabled {
		return ErrVertexProvider
	}
	if state.FoundryEnabled {
		return ErrFoundryProvider
	}
	if state.HasJSONCComments {
		return ErrIncompatibleJSONCComments
	}
	if state.ExistingBaseURL != "" && !state.ExistingBaseURLMatches {
		return fmt.Errorf("%w: existing value is %q", ErrPreExistingRedirect, state.ExistingBaseURL)
	}

	// Resolve symlink target (bounded to ~/.claude/).
	claudeDir := filepath.Dir(s.SettingsPath)
	var writePath string
	if state.SettingsFileExists {
		writePath, err = resolveSymlinkTarget(s.SettingsPath, claudeDir)
		if err != nil {
			return err
		}
	} else {
		writePath = s.SettingsPath
		if err := os.MkdirAll(claudeDir, 0o700); err != nil {
			return fmt.Errorf("settingsjson: mkdir %s: %w", claudeDir, err)
		}
	}

	// Read existing content (if any) and merge ANTHROPIC_BASE_URL.
	merged, err := mergeWithBaseURL(writePath, state.SettingsFileExists, sidecarURL)
	if err != nil {
		return err
	}

	// Write the rollback journal BEFORE the settings write. If the settings
	// write fails, the journal is harmless. If the settings write succeeds
	// but the journal write failed, we'd have an unrecoverable state on
	// exit. Order matters: journal first so "network mode ⇒ journal" always
	// holds when we look at settings.json alone.
	journal := RollbackJournal{
		EnteredAt:  time.Now().Unix(),
		SidecarURL: sidecarURL,
		SessionID:  sessionID,
		PreFallback: PreFallback{
			BaseURLWasSet:       state.ExistingBaseURL != "",
			BaseURLPriorValue:   state.ExistingBaseURL,
			SettingsFileExisted: state.SettingsFileExists,
		},
	}
	if err := writeRollback(s.RollbackPath, journal); err != nil {
		return err
	}

	if err := atomicWriteFile(writePath, merged, 0o600); err != nil {
		// best-effort: clean up the journal so state is consistent
		_ = deleteRollback(s.RollbackPath)
		return err
	}

	return nil
}

// mergeWithBaseURL reads existing settings.json (or starts fresh), merges
// env.ANTHROPIC_BASE_URL=sidecarURL, and returns pretty-printed bytes.
// Preserves every other top-level key and every other env entry verbatim.
func mergeWithBaseURL(path string, exists bool, sidecarURL string) ([]byte, error) {
	var root map[string]any
	if exists {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("settingsjson: read %s: %w", path, err)
		}
		if err := json.Unmarshal(data, &root); err != nil {
			return nil, fmt.Errorf("settingsjson: parse %s: %w", path, err)
		}
	}
	if root == nil {
		root = map[string]any{}
	}

	envRaw, ok := root["env"]
	var env map[string]any
	if ok {
		env, ok = envRaw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("settingsjson: %s: env is not an object", path)
		}
	} else {
		env = map[string]any{}
	}
	env["ANTHROPIC_BASE_URL"] = sidecarURL
	root["env"] = env

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("settingsjson: marshal merged: %w", err)
	}
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return out, nil
}

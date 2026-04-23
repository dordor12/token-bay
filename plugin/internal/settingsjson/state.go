package settingsjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/tailscale/hujson"
)

// State reports the observed state of settings.json at a point in time.
type State struct {
	SettingsFileExists     bool
	HasJSONCComments       bool
	ExistingBaseURL        string
	ExistingBaseURLMatches bool
	BedrockEnabled         bool
	VertexEnabled          bool
	FoundryEnabled         bool
	InNetworkMode          bool // set in Task 7 when the rollback journal exists and URL matches
}

// GetState reads settings.json and classifies relevant fields against
// the expected sidecar URL.
func (s *Store) GetState(sidecarURL string) (*State, error) {
	data, err := os.ReadFile(s.SettingsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &State{SettingsFileExists: false}, nil
		}
		return nil, fmt.Errorf("settingsjson: read %s: %w", s.SettingsPath, err)
	}

	out := &State{SettingsFileExists: true}

	// hasJSONCComments internally copies before calling Standardize (which
	// mutates in place), so data is untouched after this call.
	hasComments, err := hasJSONCComments(data)
	if err != nil {
		return nil, fmt.Errorf("settingsjson: parse %s: %w", s.SettingsPath, err)
	}
	out.HasJSONCComments = hasComments

	// We still need to parse for field inspection. Standardize a fresh copy
	// (hujson.Standardize is in-place so we must copy first to avoid
	// hidden aliasing bugs elsewhere).
	fresh := make([]byte, len(data))
	copy(fresh, data)
	standardized, err := hujson.Standardize(fresh)
	if err != nil {
		return nil, fmt.Errorf("settingsjson: standardize %s: %w", s.SettingsPath, err)
	}

	var parsed struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(standardized, &parsed); err != nil {
		return nil, fmt.Errorf("settingsjson: unmarshal %s: %w", s.SettingsPath, err)
	}

	out.ExistingBaseURL = parsed.Env["ANTHROPIC_BASE_URL"]
	out.ExistingBaseURLMatches = (out.ExistingBaseURL == sidecarURL)
	out.BedrockEnabled = isEnvTruthy(parsed.Env["CLAUDE_CODE_USE_BEDROCK"])
	out.VertexEnabled = isEnvTruthy(parsed.Env["CLAUDE_CODE_USE_VERTEX"])
	out.FoundryEnabled = isEnvTruthy(parsed.Env["CLAUDE_CODE_USE_FOUNDRY"])

	// InNetworkMode: rollback journal exists AND our redirect is live.
	if out.ExistingBaseURLMatches {
		if _, err := os.Stat(s.RollbackPath); err == nil {
			out.InNetworkMode = true
		}
	}

	return out, nil
}

// isEnvTruthy matches Claude Code's isEnvTruthy semantics: non-empty and
// not one of "0", "false", "no", "off" (case-insensitive).
func isEnvTruthy(v string) bool {
	if v == "" {
		return false
	}
	switch v {
	case "0", "false", "no", "off", "FALSE", "NO", "OFF", "False", "No", "Off":
		return false
	}
	return true
}

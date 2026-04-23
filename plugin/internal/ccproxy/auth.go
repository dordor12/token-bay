package ccproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// AuthState mirrors the JSON output of `claude auth status --json`.
// See src/cli/handlers/auth.ts:293-316 upstream for the authoritative
// field list.
type AuthState struct {
	LoggedIn         bool   `json:"loggedIn"`
	AuthMethod       string `json:"authMethod"`
	APIProvider      string `json:"apiProvider"`
	APIKeySource     string `json:"apiKeySource,omitempty"`
	Email            string `json:"email,omitempty"`
	OrgID            string `json:"orgId,omitempty"`
	OrgName          string `json:"orgName,omitempty"`
	SubscriptionType string `json:"subscriptionType,omitempty"`
}

// AuthProber runs `claude auth status --json` and returns the parsed state.
// Injectable so tests can substitute a stub.
type AuthProber interface {
	Probe(ctx context.Context) (*AuthState, error)
}

// ClaudeAuthProber shells out to the real claude binary.
type ClaudeAuthProber struct {
	BinaryPath string
	Timeout    time.Duration
}

// NewClaudeAuthProber returns a prober with defaults.
func NewClaudeAuthProber() *ClaudeAuthProber {
	return &ClaudeAuthProber{
		BinaryPath: "claude",
		Timeout:    5 * time.Second,
	}
}

// Probe runs `claude auth status --json` and parses stdout. Non-zero
// exit codes (user not logged in) are expected — they reflect in
// AuthState.LoggedIn, not as a Probe error. A Probe error is returned
// only when the command can't run or its stdout isn't valid JSON.
func (p *ClaudeAuthProber) Probe(ctx context.Context) (*AuthState, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.BinaryPath, "auth", "status", "--json")
	out, err := cmd.Output()
	// `claude auth status --json` exits 1 when not logged in; its stdout
	// is still valid JSON with loggedIn:false. We tolerate that exit code
	// but surface any other error (command not found, context cancelled,
	// etc.) to the caller.
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return nil, fmt.Errorf("ccproxy: run %s auth status: %w", p.BinaryPath, err)
	}

	var state AuthState
	if jerr := json.Unmarshal(out, &state); jerr != nil {
		return nil, fmt.Errorf("ccproxy: parse auth status JSON: %w", jerr)
	}
	return &state, nil
}

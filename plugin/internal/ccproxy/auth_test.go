package ccproxy

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthState_UnmarshalFullFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/auth_status_max.json")
	require.NoError(t, err, "regenerate fixture: claude auth status --json > testdata/auth_status_max.json (redact PII)")

	var state AuthState
	require.NoError(t, json.Unmarshal(data, &state))

	assert.True(t, state.LoggedIn)
	assert.Equal(t, "claude.ai", state.AuthMethod)
	assert.Equal(t, "firstParty", state.APIProvider)
	assert.Equal(t, "fixture-user@example.com", state.Email)
	assert.Equal(t, "00000000-0000-4000-8000-000000000001", state.OrgID)
	assert.Equal(t, "Fixture User's Organization", state.OrgName)
	assert.Equal(t, "max", state.SubscriptionType)
}

func TestAuthState_UnmarshalMinimalNotLoggedIn(t *testing.T) {
	data := []byte(`{"loggedIn": false, "authMethod": "none", "apiProvider": "firstParty"}`)

	var state AuthState
	require.NoError(t, json.Unmarshal(data, &state))

	assert.False(t, state.LoggedIn)
	assert.Equal(t, "none", state.AuthMethod)
	assert.Empty(t, state.Email)
	assert.Empty(t, state.OrgID)
}

func TestClaudeAuthProber_Defaults(t *testing.T) {
	p := NewClaudeAuthProber()
	assert.Equal(t, "claude", p.BinaryPath)
	assert.Equal(t, 5*time.Second, p.Timeout)
}

func TestClaudeAuthProber_MissingBinary_ReturnsError(t *testing.T) {
	p := NewClaudeAuthProber()
	p.BinaryPath = "/definitely/does/not/exist/claude"

	_, err := p.Probe(context.Background())
	assert.Error(t, err)
}

// Compile-time interface compliance.
var _ AuthProber = (*ClaudeAuthProber)(nil)

func TestClaudeAuthProber_LiveSmokeTest(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude not on PATH; skipping live smoke")
	}
	p := NewClaudeAuthProber()

	state, err := p.Probe(context.Background())
	// Exit code 1 from `claude auth status` (not logged in) is reflected
	// in state.LoggedIn, not as a Probe error. Either way the JSON is
	// valid and parseable.
	require.NoError(t, err, "probe should parse output regardless of login state")
	require.NotNil(t, state)
	assert.NotEmpty(t, state.AuthMethod, "authMethod should be populated")
}

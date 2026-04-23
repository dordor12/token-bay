package ccproxy

import (
	"encoding/json"
	"os"
	"testing"

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

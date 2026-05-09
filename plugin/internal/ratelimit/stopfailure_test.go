package ratelimit

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseStopFailurePayload_RateLimitMinimal_Succeeds(t *testing.T) {
	body := []byte(`{
		"session_id": "s-1",
		"transcript_path": "/tmp/t",
		"cwd": "/work",
		"hook_event_name": "StopFailure",
		"error": "rate_limit"
	}`)
	p, err := ParseStopFailurePayload(bytes.NewReader(body))
	require.NoError(t, err)
	assert.Equal(t, "s-1", p.SessionID)
	assert.Equal(t, "/tmp/t", p.TranscriptPath)
	assert.Equal(t, "/work", p.CWD)
	assert.Equal(t, "StopFailure", p.HookEventName)
	assert.Equal(t, ErrorRateLimit, p.Error)
	assert.Nil(t, p.PermissionMode)
	assert.Nil(t, p.AgentID)
	assert.Nil(t, p.AgentType)
	assert.Nil(t, p.ErrorDetails)
	assert.Nil(t, p.LastAssistantMessage)
}

func TestParseStopFailurePayload_WithAllOptionalFields_ParsesAll(t *testing.T) {
	body := []byte(`{
		"session_id": "s-1",
		"transcript_path": "/tmp/t",
		"cwd": "/work",
		"permission_mode": "default",
		"agent_id": "a-1",
		"agent_type": "general-purpose",
		"hook_event_name": "StopFailure",
		"error": "rate_limit",
		"error_details": "429 too many requests",
		"last_assistant_message": "I was saying..."
	}`)
	p, err := ParseStopFailurePayload(bytes.NewReader(body))
	require.NoError(t, err)
	require.NotNil(t, p.PermissionMode)
	assert.Equal(t, "default", *p.PermissionMode)
	require.NotNil(t, p.AgentID)
	assert.Equal(t, "a-1", *p.AgentID)
	require.NotNil(t, p.AgentType)
	assert.Equal(t, "general-purpose", *p.AgentType)
	require.NotNil(t, p.ErrorDetails)
	assert.Equal(t, "429 too many requests", *p.ErrorDetails)
	require.NotNil(t, p.LastAssistantMessage)
	assert.Equal(t, "I was saying...", *p.LastAssistantMessage)
}

func TestParseStopFailurePayload_MalformedJSON_ReturnsError(t *testing.T) {
	_, err := ParseStopFailurePayload(bytes.NewReader([]byte(`{not json`)))
	assert.Error(t, err)
}

func TestParseStopFailurePayload_WrongHookEvent_ReturnsError(t *testing.T) {
	body := []byte(`{
		"session_id": "s-1",
		"transcript_path": "/tmp/t",
		"cwd": "/work",
		"hook_event_name": "Stop",
		"error": "rate_limit"
	}`)
	_, err := ParseStopFailurePayload(bytes.NewReader(body))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "Stop")
}

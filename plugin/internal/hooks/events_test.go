package hooks

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSessionStart_AllFields_Succeeds(t *testing.T) {
	body := []byte(`{
		"session_id": "s-1",
		"transcript_path": "/tmp/t.jsonl",
		"cwd": "/work",
		"permission_mode": "default",
		"hook_event_name": "SessionStart",
		"source": "startup",
		"agent_type": "general",
		"model": "claude-sonnet-4-6"
	}`)

	p, err := ParseSessionStart(bytes.NewReader(body))
	require.NoError(t, err)
	assert.Equal(t, "s-1", p.SessionID)
	assert.Equal(t, "/tmp/t.jsonl", p.TranscriptPath)
	assert.Equal(t, "/work", p.CWD)
	assert.Equal(t, EventNameSessionStart, p.HookEventName)
	assert.Equal(t, "startup", p.Source)
	require.NotNil(t, p.PermissionMode)
	assert.Equal(t, "default", *p.PermissionMode)
	require.NotNil(t, p.AgentType)
	assert.Equal(t, "general", *p.AgentType)
	require.NotNil(t, p.Model)
	assert.Equal(t, "claude-sonnet-4-6", *p.Model)
}

func TestParseSessionStart_MinimalFields_Succeeds(t *testing.T) {
	body := []byte(`{
		"session_id": "s-1",
		"transcript_path": "/tmp/t.jsonl",
		"cwd": "/work",
		"hook_event_name": "SessionStart",
		"source": "resume"
	}`)
	p, err := ParseSessionStart(bytes.NewReader(body))
	require.NoError(t, err)
	assert.Equal(t, "resume", p.Source)
	assert.Nil(t, p.PermissionMode)
	assert.Nil(t, p.AgentType)
	assert.Nil(t, p.Model)
}

func TestParseSessionStart_WrongEvent_ReturnsError(t *testing.T) {
	body := []byte(`{
		"session_id": "s-1",
		"transcript_path": "/tmp/t.jsonl",
		"cwd": "/work",
		"hook_event_name": "Stop",
		"source": "startup"
	}`)
	_, err := ParseSessionStart(bytes.NewReader(body))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SessionStart")
}

func TestParseSessionStart_MalformedJSON_ReturnsError(t *testing.T) {
	_, err := ParseSessionStart(bytes.NewReader([]byte(`{not-json`)))
	assert.Error(t, err)
}

func TestParseSessionEnd_AllFields_Succeeds(t *testing.T) {
	body := []byte(`{
		"session_id": "s-9",
		"transcript_path": "/tmp/t.jsonl",
		"cwd": "/work",
		"hook_event_name": "SessionEnd",
		"reason": "logout"
	}`)
	p, err := ParseSessionEnd(bytes.NewReader(body))
	require.NoError(t, err)
	assert.Equal(t, "s-9", p.SessionID)
	assert.Equal(t, EventNameSessionEnd, p.HookEventName)
	assert.Equal(t, "logout", p.Reason)
}

func TestParseSessionEnd_WrongEvent_ReturnsError(t *testing.T) {
	body := []byte(`{
		"session_id": "s-9",
		"transcript_path": "/tmp/t.jsonl",
		"cwd": "/work",
		"hook_event_name": "SessionStart",
		"reason": "logout"
	}`)
	_, err := ParseSessionEnd(bytes.NewReader(body))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SessionEnd")
}

func TestParseUserPromptSubmit_AllFields_Succeeds(t *testing.T) {
	body := []byte(`{
		"session_id": "s-7",
		"transcript_path": "/tmp/t.jsonl",
		"cwd": "/work",
		"hook_event_name": "UserPromptSubmit",
		"prompt": "hello world"
	}`)
	p, err := ParseUserPromptSubmit(bytes.NewReader(body))
	require.NoError(t, err)
	assert.Equal(t, "s-7", p.SessionID)
	assert.Equal(t, EventNameUserPromptSubmit, p.HookEventName)
	assert.Equal(t, "hello world", p.Prompt)
}

func TestParseUserPromptSubmit_WrongEvent_ReturnsError(t *testing.T) {
	body := []byte(`{
		"session_id": "s-7",
		"transcript_path": "/tmp/t.jsonl",
		"cwd": "/work",
		"hook_event_name": "SessionStart",
		"prompt": "hi"
	}`)
	_, err := ParseUserPromptSubmit(bytes.NewReader(body))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UserPromptSubmit")
}

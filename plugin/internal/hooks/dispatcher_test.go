package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodeResponse(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	out := map[string]any{}
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func TestDispatcher_StopFailure_HappyPath(t *testing.T) {
	rec := NewRecordingSink()
	d := &Dispatcher{Sink: rec}

	body := []byte(`{
		"session_id": "s-1",
		"transcript_path": "/tmp/t",
		"cwd": "/work",
		"hook_event_name": "StopFailure",
		"error": "rate_limit"
	}`)

	var out bytes.Buffer
	err := d.Handle(context.Background(), EventNameStopFailure, bytes.NewReader(body), &out)
	require.NoError(t, err)

	require.Len(t, rec.StopFailures, 1)
	assert.Equal(t, "s-1", rec.StopFailures[0].SessionID)
	assert.Equal(t, "rate_limit", rec.StopFailures[0].Error)
	assert.Equal(t, map[string]any{}, decodeResponse(t, out.Bytes()))
}

func TestDispatcher_SessionStart_HappyPath(t *testing.T) {
	rec := NewRecordingSink()
	d := &Dispatcher{Sink: rec}

	body := []byte(`{
		"session_id": "s-2",
		"transcript_path": "/tmp/t",
		"cwd": "/work",
		"hook_event_name": "SessionStart",
		"source": "startup"
	}`)
	var out bytes.Buffer
	require.NoError(t, d.Handle(context.Background(), EventNameSessionStart, bytes.NewReader(body), &out))

	require.Len(t, rec.SessionStarts, 1)
	assert.Equal(t, "s-2", rec.SessionStarts[0].SessionID)
	assert.Equal(t, "startup", rec.SessionStarts[0].Source)
	assert.Equal(t, map[string]any{}, decodeResponse(t, out.Bytes()))
}

func TestDispatcher_SessionEnd_HappyPath(t *testing.T) {
	rec := NewRecordingSink()
	d := &Dispatcher{Sink: rec}

	body := []byte(`{
		"session_id": "s-3",
		"transcript_path": "/tmp/t",
		"cwd": "/work",
		"hook_event_name": "SessionEnd",
		"reason": "logout"
	}`)
	var out bytes.Buffer
	require.NoError(t, d.Handle(context.Background(), EventNameSessionEnd, bytes.NewReader(body), &out))

	require.Len(t, rec.SessionEnds, 1)
	assert.Equal(t, "logout", rec.SessionEnds[0].Reason)
}

func TestDispatcher_UserPromptSubmit_HappyPath(t *testing.T) {
	rec := NewRecordingSink()
	d := &Dispatcher{Sink: rec}

	body := []byte(`{
		"session_id": "s-4",
		"transcript_path": "/tmp/t",
		"cwd": "/work",
		"hook_event_name": "UserPromptSubmit",
		"prompt": "ping"
	}`)
	var out bytes.Buffer
	require.NoError(t, d.Handle(context.Background(), EventNameUserPromptSubmit, bytes.NewReader(body), &out))

	require.Len(t, rec.UserPromptSubmits, 1)
	assert.Equal(t, "ping", rec.UserPromptSubmits[0].Prompt)
}

func TestDispatcher_UnknownEvent_ReturnsError(t *testing.T) {
	d := &Dispatcher{Sink: &NopSink{}}
	var out bytes.Buffer
	err := d.Handle(context.Background(), "PreToolUse", strings.NewReader(`{}`), &out)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnknownEvent)
	// On unknown event we do NOT write a response — caller decides what to
	// do with stdout (likely close it without output).
	assert.Empty(t, out.Bytes())
}

func TestDispatcher_MalformedJSON_ReturnsError_NoResponse(t *testing.T) {
	rec := NewRecordingSink()
	d := &Dispatcher{Sink: rec}

	var out bytes.Buffer
	err := d.Handle(context.Background(), EventNameStopFailure, strings.NewReader(`{not json`), &out)
	require.Error(t, err)
	assert.Empty(t, rec.StopFailures)
	assert.Empty(t, out.Bytes())
}

func TestDispatcher_PayloadEventNameMismatch_ReturnsError(t *testing.T) {
	d := &Dispatcher{Sink: &NopSink{}}
	body := []byte(`{
		"session_id": "s-9",
		"transcript_path": "/tmp/t",
		"cwd": "/work",
		"hook_event_name": "Stop",
		"reason": "logout"
	}`)
	var out bytes.Buffer
	err := d.Handle(context.Background(), EventNameSessionEnd, bytes.NewReader(body), &out)
	require.Error(t, err)
	assert.Empty(t, out.Bytes())
}

func TestDispatcher_SinkError_PropagatesAndStillWritesResponse(t *testing.T) {
	rec := NewRecordingSink()
	rec.Err = errors.New("ipc-down")
	d := &Dispatcher{Sink: rec}

	body := []byte(`{
		"session_id": "s-1",
		"transcript_path": "/tmp/t",
		"cwd": "/work",
		"hook_event_name": "StopFailure",
		"error": "rate_limit"
	}`)
	var out bytes.Buffer
	err := d.Handle(context.Background(), EventNameStopFailure, bytes.NewReader(body), &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ipc-down")

	// Response is still written so the host Claude Code is never blocked
	// by a sink failure: the plugin's spec is that hooks observe and
	// optionally coordinate — never block the host turn.
	assert.Equal(t, map[string]any{}, decodeResponse(t, out.Bytes()))
	assert.Len(t, rec.StopFailures, 1)
}

func TestDispatcher_NilSink_ReturnsErrInvalidConfig(t *testing.T) {
	d := &Dispatcher{}
	var out bytes.Buffer
	err := d.Handle(context.Background(), EventNameStopFailure, strings.NewReader(`{}`), &out)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidConfig)
	assert.Empty(t, out.Bytes())
}

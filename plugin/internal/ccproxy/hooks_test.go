package ccproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/hooks"
)

// TestServer_HooksEndpoint_DispatchesToInjectedSink confirms that a POST to
// /_hooks/StopFailure delivers the parsed payload to the Sink that the
// sidecar wires in via WithHookSink. The Sink contract (hooks.Sink) is the
// integration seam between the long-lived sidecar and the per-event hook
// subprocess; without dispatch the consumerflow.Coordinator never sees any
// events at all.
func TestServer_HooksEndpoint_DispatchesToInjectedSink(t *testing.T) {
	sink := hooks.NewRecordingSink()
	s := New(
		WithAddr("127.0.0.1:0"),
		WithHookSink(sink),
	)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = s.Close()
	})
	require.NoError(t, s.Start(ctx))

	body := `{"session_id":"sess-1","transcript_path":"/tmp/x","cwd":"/tmp","hook_event_name":"StopFailure","error":"rate_limit"}`
	resp, err := http.Post(s.URL()+"_hooks/StopFailure", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	// The contract is that the dispatcher always emits an EmptyResponse JSON
	// body even when sinks are slow or absent — so the host turn stays clean.
	respBody, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(respBody), "{")

	require.Len(t, sink.StopFailures, 1)
	assert.Equal(t, "sess-1", sink.StopFailures[0].SessionID)
	assert.Equal(t, "rate_limit", sink.StopFailures[0].Error)
}

// TestServer_HooksEndpoint_NoSink_ReturnsEmptyResponse confirms the hook
// route degrades gracefully when no Sink is wired (e.g. a misconfigured
// sidecar): always 200 + EmptyResponse, never 5xx, never blocks the host.
func TestServer_HooksEndpoint_NoSink_ReturnsEmptyResponse(t *testing.T) {
	s := New(WithAddr("127.0.0.1:0"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = s.Close()
	})
	require.NoError(t, s.Start(ctx))

	body := `{"hook_event_name":"SessionStart","session_id":"x","transcript_path":"/tmp/x","cwd":"/tmp","source":"startup"}`
	resp, err := http.Post(s.URL()+"_hooks/SessionStart", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestServer_HooksEndpoint_UnknownEvent_400 confirms parse/dispatch errors
// surface as 400 — the hook subprocess uses the status as a signal to
// stderr-log without modifying the host turn (the empty-Response body is
// still returned).
func TestServer_HooksEndpoint_UnknownEvent_400(t *testing.T) {
	sink := hooks.NewRecordingSink()
	s := New(WithAddr("127.0.0.1:0"), WithHookSink(sink))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = s.Close()
	})
	require.NoError(t, s.Start(ctx))

	resp, err := http.Post(s.URL()+"_hooks/Bogus", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Empty(t, sink.StopFailures)
	assert.Empty(t, sink.SessionStarts)
}

// TestServer_HooksEndpoint_SinkError_StillEmits200 holds the wire contract:
// the host turn must never be blocked, so even a Sink that returns an error
// produces a 200 EmptyResponse on the wire.
func TestServer_HooksEndpoint_SinkError_StillEmits200(t *testing.T) {
	sink := hooks.NewRecordingSink()
	sink.Err = io.ErrUnexpectedEOF // arbitrary non-nil
	s := New(WithAddr("127.0.0.1:0"), WithHookSink(sink))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = s.Close()
	})
	require.NoError(t, s.Start(ctx))

	body := `{"hook_event_name":"SessionEnd","session_id":"x","transcript_path":"/tmp/x","cwd":"/tmp","reason":"clear"}`
	resp, err := http.Post(s.URL()+"_hooks/SessionEnd", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	require.Len(t, sink.SessionEnds, 1)
}

// TestServer_HooksRoute_DoesNotShadowAnthropic confirms /_hooks/* is its own
// explicit mux entry — Plugin CLAUDE.md rule #3 requires /v1/messages
// continue to flow into the existing PassThrough/Network handler unchanged.
func TestServer_HooksRoute_DoesNotShadowAnthropic(t *testing.T) {
	pass := &recordingRouter{status: http.StatusOK, body: "pass-through-hit"}
	s := New(
		WithAddr("127.0.0.1:0"),
		WithPassThroughRouter(pass),
		WithHookSink(hooks.NewRecordingSink()),
	)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = s.Close()
	})
	require.NoError(t, s.Start(ctx))

	resp, err := http.Post(s.URL()+"v1/messages", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	assert.Equal(t, "pass-through-hit", string(out))
	assert.Equal(t, 1, pass.called)
}

// TestServer_StatusEndpoint_ReturnsSnapshot covers the slash-command side
// of the IPC: /token-bay status reads this endpoint to render running-state.
func TestServer_StatusEndpoint_ReturnsSnapshot(t *testing.T) {
	s := New(WithAddr("127.0.0.1:0"))
	s.SetStatusProvider(func() map[string]any {
		return map[string]any{"running": true, "tracker_state": "connected"}
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = s.Close()
	})
	require.NoError(t, s.Start(ctx))

	resp, err := http.Get(s.URL() + "_status")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var got map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	assert.Equal(t, true, got["running"])
	assert.Equal(t, "connected", got["tracker_state"])
}

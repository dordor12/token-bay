package sidecar

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/hooks"
)

// TestNew_WiresHookSinkIntoCCProxy confirms the supervisor passes Deps.HookSink
// into ccproxy via WithHookSink so the running /_hooks/{event} HTTP route
// dispatches events into the configured Sink (production: consumerflow.Coord).
// Without this seam the per-event hook subprocess can't reach the Coordinator.
func TestNew_WiresHookSinkIntoCCProxy(t *testing.T) {
	deps := validDeps(t)
	sink := hooks.NewRecordingSink()
	deps.HookSink = sink

	app, err := New(deps)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = app.Run(ctx) }()

	// Wait for ccproxy to bind.
	require.Eventually(t, func() bool {
		return app.proxy.URL() != ""
	}, 2*time.Second, 10*time.Millisecond)

	url := app.proxy.URL() + "_hooks/SessionStart"
	body := `{"hook_event_name":"SessionStart","session_id":"test","transcript_path":"/tmp/x","cwd":"/tmp","source":"startup"}`
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	require.Eventually(t, func() bool {
		return len(sink.SessionStarts) == 1
	}, time.Second, 10*time.Millisecond, "sink should observe the SessionStart event")
}

package sidecar

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/hooks"
)

func TestRun_WiresHookSink_ReceivesStopFailureFromCCProxy(t *testing.T) {
	deps := validDeps(t)
	sink := hooks.NewRecordingSink()
	deps.HookSink = sink

	app, err := New(deps)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run(ctx) }()

	// Wait for ccproxy URL to become available.
	require.Eventually(t, func() bool {
		return app.Status().CCProxyURL != ""
	}, time.Second, 10*time.Millisecond)

	body := `{"hook_event_name":"StopFailure","session_id":"abc","transcript_path":"/tmp/x","cwd":"/tmp","stop_hook_active":true,"reason":"Claude AI usage limit reached|1700000000"}`
	resp, err := http.Post(app.Status().CCProxyURL+"_hooks/StopFailure", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	require.Eventually(t, func() bool {
		return len(sink.StopFailures) == 1
	}, time.Second, 10*time.Millisecond)

	cancel()
	<-errCh
}

func TestRun_WiresStatusProvider_ReturnsAppSnapshot(t *testing.T) {
	deps := validDeps(t)
	app, err := New(deps)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- app.Run(ctx) }()

	require.Eventually(t, func() bool {
		return app.Status().CCProxyURL != ""
	}, time.Second, 10*time.Millisecond)

	resp, err := http.Get(app.Status().CCProxyURL + "_status")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var snap map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&snap))
	assert.Equal(t, true, snap["running"])
	assert.Contains(t, snap["ccproxy_url"], "127.0.0.1")

	cancel()
	<-errCh
}

func TestRun_BalanceProvider_OptionalBalanceFnReturnsSnapshot(t *testing.T) {
	deps := validDeps(t)
	deps.BalanceFn = func(_ context.Context) (credits int64, lastUpdated time.Time, source string, err error) {
		return 4242, time.Unix(1_700_000_000, 0).UTC(), "cached", nil
	}
	app, err := New(deps)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- app.Run(ctx) }()

	require.Eventually(t, func() bool {
		return app.Status().CCProxyURL != ""
	}, time.Second, 10*time.Millisecond)

	resp, err := http.Get(app.Status().CCProxyURL + "_balance")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var snap map[string]any
	require.NoError(t, json.Unmarshal(body, &snap))
	assert.Equal(t, float64(4242), snap["credits"])
	assert.Equal(t, "cached", snap["source"])

	cancel()
	<-errCh
}

func TestRun_NoBalanceFn_BalanceEndpoint503(t *testing.T) {
	deps := validDeps(t)
	app, err := New(deps)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- app.Run(ctx) }()

	require.Eventually(t, func() bool {
		return app.Status().CCProxyURL != ""
	}, time.Second, 10*time.Millisecond)

	resp, err := http.Get(app.Status().CCProxyURL + "_balance")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	cancel()
	<-errCh
}

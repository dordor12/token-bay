package ccproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/hooks"
	"github.com/token-bay/token-bay/plugin/internal/ratelimit"
)

// blockingSink fails every call so the dispatcher's "always write
// EmptyResponse" contract is observable.
type blockingSink struct {
	hooks.NopSink
	err error
}

func (b *blockingSink) OnStopFailure(ctx context.Context, p *ratelimit.StopFailurePayload) error {
	return b.err
}

func TestServer_HooksStopFailure_DispatchesToSink(t *testing.T) {
	sink := hooks.NewRecordingSink()
	s := New(WithAddr("127.0.0.1:0"), WithHookSink(sink))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = s.Close() })
	require.NoError(t, s.Start(ctx))

	body := `{"hook_event_name":"StopFailure","session_id":"abc","transcript_path":"/tmp/x","cwd":"/tmp","stop_hook_active":true,"reason":"Claude AI usage limit reached|1700000000"}`

	resp, err := http.Post(s.URL()+"_hooks/StopFailure", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	// EmptyResponse encodes as `{}\n`.
	assert.Equal(t, "{}\n", string(respBody))
	assert.Len(t, sink.StopFailures, 1)
}

func TestServer_HooksSinkError_StillReturns200WithEmptyResponse(t *testing.T) {
	sink := &blockingSink{err: assertErr("sink unhealthy")}
	s := New(WithAddr("127.0.0.1:0"), WithHookSink(sink))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = s.Close() })
	require.NoError(t, s.Start(ctx))

	body := `{"hook_event_name":"StopFailure","session_id":"abc","transcript_path":"/tmp/x","cwd":"/tmp","stop_hook_active":true,"reason":"Claude AI usage limit reached|1700000000"}`

	resp, err := http.Post(s.URL()+"_hooks/StopFailure", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "{}\n", string(respBody))
}

func TestServer_HooksUnknownEvent_ReturnsEmptyResponse(t *testing.T) {
	sink := hooks.NewRecordingSink()
	s := New(WithAddr("127.0.0.1:0"), WithHookSink(sink))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = s.Close() })
	require.NoError(t, s.Start(ctx))

	resp, err := http.Post(s.URL()+"_hooks/NotARealEvent", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "{}\n", string(respBody))
}

func TestServer_HooksRoute_DoesNotFallThroughToUpstream(t *testing.T) {
	// PassThrough router with a recording call counter — if /_hooks/* ever
	// fell through, this would tick.
	pass := &recordingRouter{status: http.StatusOK, body: "from-pass"}
	sink := hooks.NewRecordingSink()
	s := New(WithAddr("127.0.0.1:0"), WithPassThroughRouter(pass), WithHookSink(sink))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = s.Close() })
	require.NoError(t, s.Start(ctx))

	body := `{"hook_event_name":"SessionStart","session_id":"abc","transcript_path":"/tmp/x","cwd":"/tmp","source":"startup"}`
	resp, err := http.Post(s.URL()+"_hooks/SessionStart", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, 0, pass.called, "/_hooks/* must not fall through to PassThrough")
}

func TestServer_StatusEndpoint_ReturnsInjectedSnapshot(t *testing.T) {
	s := New(WithAddr("127.0.0.1:0"))
	called := int32(0)
	s.SetStatusProvider(func() any {
		atomic.AddInt32(&called, 1)
		return map[string]any{
			"running":     true,
			"ccproxy_url": "http://127.0.0.1:1234/",
			"tracker":     "connected",
			"audit_log":   "/tmp/audit.log",
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = s.Close() })
	require.NoError(t, s.Start(ctx))

	resp, err := http.Get(s.URL() + "_status")
	require.NoError(t, err)
	defer resp.Body.Close()

	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Equal(t, true, out["running"])
	assert.Equal(t, "/tmp/audit.log", out["audit_log"])
	assert.Equal(t, int32(1), atomic.LoadInt32(&called))
}

func TestServer_StatusEndpoint_NoProvider_Returns503(t *testing.T) {
	s := New(WithAddr("127.0.0.1:0"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = s.Close() })
	require.NoError(t, s.Start(ctx))

	resp, err := http.Get(s.URL() + "_status")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestServer_BalanceEndpoint_ReturnsInjectedSnapshot(t *testing.T) {
	s := New(WithAddr("127.0.0.1:0"))
	s.SetBalanceProvider(func() any {
		return map[string]any{
			"credits":      int64(1234),
			"last_updated": int64(1700000000),
			"source":       "cached",
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = s.Close() })
	require.NoError(t, s.Start(ctx))

	resp, err := http.Get(s.URL() + "_balance")
	require.NoError(t, err)
	defer resp.Body.Close()

	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Equal(t, float64(1234), out["credits"])
	assert.Equal(t, "cached", out["source"])
}

func TestServer_BalanceEndpoint_NoProvider_Returns503(t *testing.T) {
	s := New(WithAddr("127.0.0.1:0"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = s.Close() })
	require.NoError(t, s.Start(ctx))

	resp, err := http.Get(s.URL() + "_balance")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestServer_V1MessagesStillWorks_WithHookEndpointsMounted(t *testing.T) {
	pass := &recordingRouter{status: http.StatusOK, body: "from-pass"}
	netr := &recordingRouter{status: http.StatusOK, body: "from-net"}
	sink := hooks.NewRecordingSink()
	s := New(
		WithAddr("127.0.0.1:0"),
		WithPassThroughRouter(pass),
		WithNetworkRouter(netr),
		WithHookSink(sink),
	)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = s.Close() })
	require.NoError(t, s.Start(ctx))

	resp, err := http.Post(s.URL()+"v1/messages", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, "from-pass", string(body))
}

// assertErr is a tiny helper to avoid fmt.Errorf imports in test files.
type errString string

func (e errString) Error() string { return string(e) }

func assertErr(s string) error { return errString(s) }

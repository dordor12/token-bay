package ccproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingRouter struct {
	mu     sync.Mutex
	called int
	status int
	body   string
}

func (r *recordingRouter) Route(w http.ResponseWriter, req *http.Request, _ *EntryMetadata) {
	r.mu.Lock()
	r.called++
	r.mu.Unlock()
	w.WriteHeader(r.status)
	_, _ = w.Write([]byte(r.body))
}

func newServerForTest(t *testing.T) (*Server, *recordingRouter, *recordingRouter) {
	t.Helper()
	pass := &recordingRouter{status: http.StatusOK, body: "from-pass"}
	net := &recordingRouter{status: http.StatusOK, body: "from-net"}
	s := New(
		WithAddr("127.0.0.1:0"),
		WithPassThroughRouter(pass),
		WithNetworkRouter(net),
	)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = s.Close()
	})
	require.NoError(t, s.Start(ctx))
	return s, pass, net
}

func TestServer_StartBindsDynamicPort(t *testing.T) {
	s, _, _ := newServerForTest(t)
	assert.NotEmpty(t, s.URL())
	assert.Contains(t, s.URL(), "127.0.0.1")
}

func TestServer_UnknownSession_RoutesPassThrough(t *testing.T) {
	s, pass, net := newServerForTest(t)
	req, _ := http.NewRequest(http.MethodPost, s.URL()+"v1/messages", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("X-Claude-Code-Session-Id", "unknown-session")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "from-pass", string(body))
	assert.Equal(t, 1, pass.called)
	assert.Equal(t, 0, net.called)
}

func TestServer_NetworkModeSession_RoutesNetwork(t *testing.T) {
	s, pass, net := newServerForTest(t)
	s.Store.EnterNetworkMode("net-session", EntryMetadata{
		EnteredAt: time.Now(),
		ExpiresAt: time.Now().Add(10 * time.Minute),
	})

	req, _ := http.NewRequest(http.MethodPost, s.URL()+"v1/messages", strings.NewReader(`{}`))
	req.Header.Set("X-Claude-Code-Session-Id", "net-session")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	assert.Equal(t, "from-net", string(body))
	assert.Equal(t, 1, net.called)
	assert.Equal(t, 0, pass.called)
}

func TestServer_MissingSessionHeader_RoutesPassThrough(t *testing.T) {
	s, pass, _ := newServerForTest(t)
	resp, err := http.Post(s.URL()+"v1/messages", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 1, pass.called)
}

func TestServer_HealthEndpoint_ReturnsOK(t *testing.T) {
	s, _, _ := newServerForTest(t)
	resp, err := http.Get(s.URL() + "token-bay/health")
	require.NoError(t, err)
	defer resp.Body.Close()

	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "ok", body["status"])
	assert.Contains(t, body["addr"], "127.0.0.1")
}

func TestServer_CleanClose_NoError(t *testing.T) {
	s := New(WithAddr("127.0.0.1:0"))
	require.NoError(t, s.Start(context.Background()))
	assert.NoError(t, s.Close())
}

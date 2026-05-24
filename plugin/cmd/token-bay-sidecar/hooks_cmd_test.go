package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runHooksCmd executes the hooks subcommand against the given stdin and
// returns stdout, stderr, and the process exit error (nil = success).
func runHooksCmd(t *testing.T, cfgDir, event string, stdin string) (string, string, error) {
	t.Helper()
	cmd := newRootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs([]string{"hooks", event, "--config", filepath.Join(cfgDir, "config.yaml")})
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestHooksCmd_MissingDiscoveryFile_WritesEmptyResponseAndExits0(t *testing.T) {
	dir := t.TempDir()
	// No sidecar.url present.

	stdout, _, err := runHooksCmd(t, dir, "StopFailure", `{"hook_event_name":"StopFailure","session_id":"x","transcript_path":"/tmp/x","cwd":"/tmp","stop_hook_active":true,"reason":"r"}`)
	require.NoError(t, err)
	assert.Equal(t, "{}\n", stdout)
}

func TestHooksCmd_HappyPath_CopiesSidecarResponse(t *testing.T) {
	dir := t.TempDir()

	called := int32(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
		assert.Equal(t, "/_hooks/StopFailure", r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		assert.Contains(t, string(body), "StopFailure")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"continue":true}` + "\n"))
	}))
	defer srv.Close()

	require.NoError(t, writeDiscoveryFile(dir, srv.URL+"/"))

	stdout, _, err := runHooksCmd(t, dir, "StopFailure", `{"hook_event_name":"StopFailure","session_id":"x","transcript_path":"/tmp/x","cwd":"/tmp","stop_hook_active":true,"reason":"r"}`)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.NewDecoder(strings.NewReader(stdout)).Decode(&decoded))
	assert.Equal(t, true, decoded["continue"])
	assert.Equal(t, int32(1), atomic.LoadInt32(&called))
}

func TestHooksCmd_Sidecar5xx_WritesEmptyResponseAndExits0(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	require.NoError(t, writeDiscoveryFile(dir, srv.URL+"/"))

	stdout, _, err := runHooksCmd(t, dir, "StopFailure", `{}`)
	require.NoError(t, err)
	assert.Equal(t, "{}\n", stdout)
}

func TestHooksCmd_SidecarConnectionRefused_WritesEmptyResponseAndExits0(t *testing.T) {
	dir := t.TempDir()
	// Bind a port, close it, write the (now-dead) URL. POST should
	// connection-refuse and the cmd must still emit EmptyResponse.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close()
	require.NoError(t, writeDiscoveryFile(dir, addr+"/"))

	stdout, _, err := runHooksCmd(t, dir, "StopFailure", `{}`)
	require.NoError(t, err)
	assert.Equal(t, "{}\n", stdout)
}

func TestHooksCmd_SidecarTimeout_WritesEmptyResponseAndExits0(t *testing.T) {
	dir := t.TempDir()
	// Sidecar accepts and hangs until the test signals teardown.
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	defer func() { close(stop); srv.Close() }()
	require.NoError(t, writeDiscoveryFile(dir, srv.URL+"/"))

	// Use a short timeout for the test.
	prev := hooksTimeoutOverride
	hooksTimeoutOverride = 150 * time.Millisecond
	t.Cleanup(func() { hooksTimeoutOverride = prev })

	start := time.Now()
	stdout, _, err := runHooksCmd(t, dir, "StopFailure", `{}`)
	require.Less(t, time.Since(start), 5*time.Second, "should not block past timeout")
	require.NoError(t, err)
	assert.Equal(t, "{}\n", stdout)
}

func TestHooksCmd_UnknownEvent_StillCopiesSidecarResponse(t *testing.T) {
	// The unknown-event policy lives on the sidecar (it returns
	// EmptyResponse). The cmd is a transparent proxy and so should
	// surface whatever the sidecar replies.
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}` + "\n"))
	}))
	defer srv.Close()
	require.NoError(t, writeDiscoveryFile(dir, srv.URL+"/"))

	stdout, _, err := runHooksCmd(t, dir, "Bogus", `{}`)
	require.NoError(t, err)
	assert.Equal(t, "{}\n", stdout)
}

func TestHooksCmd_RequiresConfigFlag(t *testing.T) {
	cmd := newRootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"hooks", "StopFailure"})
	err := cmd.Execute()
	require.Error(t, err)
}

func TestHooksCmd_DiscoveryFileBlank_TreatsAsMissing(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(discoveryFilename(dir), []byte(""), 0o600))

	stdout, _, err := runHooksCmd(t, dir, "StopFailure", `{}`)
	require.NoError(t, err)
	assert.Equal(t, "{}\n", stdout)
}

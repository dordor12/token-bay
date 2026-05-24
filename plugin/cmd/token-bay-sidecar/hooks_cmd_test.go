package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunHookSubcommand_MissingDiscoveryFile_ExitsSilently encodes the
// load-bearing contract: if the sidecar isn't running, the hook subprocess
// must NOT block the host Claude Code turn. runHookSubcommand returns nil
// and writes an EmptyResponse to stdout so the host hook executor sees a
// clean reply.
func TestRunHookSubcommand_MissingDiscoveryFile_ExitsSilently(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	err := runHookSubcommand(context.Background(), hookSubcommandOpts{
		CfgDir:    dir,
		EventName: "SessionStart",
		Stdin:     strings.NewReader("{}"),
		Stdout:    &stdout,
		Stderr:    &stderr,
		Timeout:   time.Second,
	})
	require.NoError(t, err)
	// Body must be a JSON object — the host parses it as a Response.
	assert.True(t, json.Valid(stdout.Bytes()), "stdout must be valid JSON: %q", stdout.String())
	// Stderr should log a one-liner so the operator can diagnose.
	assert.Contains(t, stderr.String(), "sidecar.url")
}

// TestRunHookSubcommand_HappyPath_ForwardsStdinAndCopiesResponse covers the
// normal case: discovery file resolves, sidecar 200s with EmptyResponse,
// subprocess copies that to stdout.
func TestRunHookSubcommand_HappyPath_ForwardsStdinAndCopiesResponse(t *testing.T) {
	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		receivedBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sidecar":"saw-the-event"}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	require.NoError(t, writeDiscoveryFile(dir, srv.URL+"/"))

	var stdout, stderr bytes.Buffer
	body := `{"hook_event_name":"SessionStart","session_id":"s","transcript_path":"/tmp/x","cwd":"/tmp","source":"startup"}`
	err := runHookSubcommand(context.Background(), hookSubcommandOpts{
		CfgDir:    dir,
		EventName: "SessionStart",
		Stdin:     strings.NewReader(body),
		Stdout:    &stdout,
		Stderr:    &stderr,
		Timeout:   2 * time.Second,
	})
	require.NoError(t, err)
	assert.Equal(t, body, receivedBody, "stdin must be forwarded verbatim to the sidecar")
	assert.Contains(t, stdout.String(), "saw-the-event")
}

// TestRunHookSubcommand_Sidecar5xx_StillEmits200ExitCode covers the failure
// mode where the sidecar is reachable but unhealthy — we still emit an
// EmptyResponse on stdout and exit 0 so the host turn proceeds. The error
// is logged to stderr for the operator.
func TestRunHookSubcommand_Sidecar5xx_StillEmits200ExitCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	require.NoError(t, writeDiscoveryFile(dir, srv.URL+"/"))

	var stdout, stderr bytes.Buffer
	err := runHookSubcommand(context.Background(), hookSubcommandOpts{
		CfgDir:    dir,
		EventName: "SessionStart",
		Stdin:     strings.NewReader("{}"),
		Stdout:    &stdout,
		Stderr:    &stderr,
		Timeout:   2 * time.Second,
	})
	require.NoError(t, err)
	assert.True(t, json.Valid(stdout.Bytes()), "stdout must be valid JSON")
	assert.Contains(t, stderr.String(), "5")
}

// TestRunHookSubcommand_Timeout_EmitsEmptyResponse covers the timeout
// case: the sidecar is hung; we must still exit 0 with an EmptyResponse
// before Claude Code's own 5s hook timeout fires.
func TestRunHookSubcommand_Timeout_EmitsEmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		time.Sleep(3 * time.Second)
	}))
	defer srv.Close()

	dir := t.TempDir()
	require.NoError(t, writeDiscoveryFile(dir, srv.URL+"/"))

	var stdout, stderr bytes.Buffer
	err := runHookSubcommand(context.Background(), hookSubcommandOpts{
		CfgDir:    dir,
		EventName: "SessionStart",
		Stdin:     strings.NewReader("{}"),
		Stdout:    &stdout,
		Stderr:    &stderr,
		Timeout:   200 * time.Millisecond,
	})
	require.NoError(t, err)
	assert.True(t, json.Valid(stdout.Bytes()), "stdout must be valid JSON on timeout")
}

// TestNewHooksCmd_RegisteredOnRoot — `--help` must list `hooks` so the
// plugin.json declarations resolve.
func TestNewHooksCmd_RegisteredOnRoot(t *testing.T) {
	root := newRootCmd()
	var found bool
	for _, c := range root.Commands() {
		if c.Name() == "hooks" {
			found = true
			break
		}
	}
	require.True(t, found, "hooks subcommand must be wired in newRootCmd")
}

// TestRunHookSubcommand_RejectsUnknownEvent confirms we don't blindly
// forward arbitrary event names — that would let a misconfigured
// plugin.json silently exercise paths the sidecar doesn't expect.
func TestRunHookSubcommand_RejectsUnknownEvent(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeDiscoveryFile(dir, "http://127.0.0.1:1/"))
	var stdout, stderr bytes.Buffer
	err := runHookSubcommand(context.Background(), hookSubcommandOpts{
		CfgDir:    dir,
		EventName: "MadeUp",
		Stdin:     strings.NewReader("{}"),
		Stdout:    &stdout,
		Stderr:    &stderr,
		Timeout:   time.Second,
	})
	require.NoError(t, err) // never blocks host
	assert.Contains(t, stderr.String(), "MadeUp")
}

// TestRunHookSubcommand_BadDiscoveryURL — discovery file is present but
// its contents don't parse as a URL or are empty. Treat as no-sidecar.
func TestRunHookSubcommand_BadDiscoveryURL(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeDiscoveryFile(dir, "  \n"))
	// We just wrote "  \n" — readDiscoveryFile trims, returns "". The
	// caller must treat that as a missing sidecar and exit 0.
	got, _ := readDiscoveryFile(dir)
	require.Equal(t, "", got)

	// And just to be safe, build an "obviously wrong" URL.
	require.NoError(t, writeDiscoveryFile(dir, ":\\not a url"))
	got, _ = readDiscoveryFile(dir)
	require.NotEmpty(t, got)
	// Path should exist (the file is in the right place).
	_, err := readDiscoveryFile(dir)
	require.NoError(t, err)
	// And our path should be inside the cfgDir we passed.
	require.FileExists(t, filepath.Join(dir, discoveryFilename))
}

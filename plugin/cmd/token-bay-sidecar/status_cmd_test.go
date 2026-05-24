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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeStubConfig(t *testing.T, dir string) string {
	t.Helper()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("role: both\ntracker: https://x.example:7443\n"), 0o600))
	return cfgPath
}

func runStatusCmd(t *testing.T, dir string, args ...string) (string, string, error) {
	t.Helper()
	cmd := newRootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(append([]string{"status", "--config", filepath.Join(dir, "config.yaml")}, args...))
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestStatusCmd_HappyPath_RendersTable(t *testing.T) {
	dir := t.TempDir()
	writeStubConfig(t, dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/_status", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"running":     true,
			"ccproxy_url": "http://127.0.0.1:1234/",
			"tracker":     "connected",
			"audit_log":   "/tmp/audit.log",
		})
	}))
	defer srv.Close()
	require.NoError(t, writeDiscoveryFile(dir, srv.URL+"/"))

	out, _, err := runStatusCmd(t, dir)
	require.NoError(t, err)
	assert.Contains(t, out, "running")
	assert.Contains(t, out, "true")
	assert.Contains(t, out, "tracker")
	assert.Contains(t, out, "connected")
}

func TestStatusCmd_JSONFlag_PrintsRawJSON(t *testing.T) {
	dir := t.TempDir()
	writeStubConfig(t, dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"running":true,"audit_log":"/tmp/a"}`+"\n")
	}))
	defer srv.Close()
	require.NoError(t, writeDiscoveryFile(dir, srv.URL+"/"))

	out, _, err := runStatusCmd(t, dir, "--json")
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &decoded))
	assert.Equal(t, true, decoded["running"])
}

func TestStatusCmd_SidecarDown_NonZeroExit(t *testing.T) {
	dir := t.TempDir()
	writeStubConfig(t, dir)
	// No sidecar.url written.

	_, errOut, err := runStatusCmd(t, dir)
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(errOut+err.Error()), "sidecar")
}

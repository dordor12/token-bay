package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runFallbackCmd(t *testing.T, dir string, args ...string) (string, string, error) {
	t.Helper()
	cmd := newRootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(append([]string{"fallback", "--config", filepath.Join(dir, "config.yaml")}, args...))
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestFallbackCmd_PostsSyntheticStopFailure(t *testing.T) {
	dir := t.TempDir()
	writeStubConfig(t, dir)

	called := int32(0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
		assert.Equal(t, "/_hooks/StopFailure", r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		var p map[string]any
		require.NoError(t, json.Unmarshal(body, &p))
		assert.Equal(t, "StopFailure", p["hook_event_name"])
		assert.Contains(t, p["reason"], "Claude AI usage limit reached")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`+"\n")
	}))
	defer srv.Close()
	require.NoError(t, writeDiscoveryFile(dir, srv.URL+"/"))

	out, _, err := runFallbackCmd(t, dir)
	require.NoError(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&called))
	assert.Contains(t, out, "fallback")
}

func TestFallbackCmd_SidecarDown_NonZeroExit(t *testing.T) {
	dir := t.TempDir()
	writeStubConfig(t, dir)
	_, errOut, err := runFallbackCmd(t, dir)
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(errOut+err.Error()), "sidecar")
}

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runBalanceCmd(t *testing.T, dir string, args ...string) (string, string, error) {
	t.Helper()
	cmd := newRootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(append([]string{"balance", "--config", filepath.Join(dir, "config.yaml")}, args...))
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestBalanceCmd_HappyPath_RendersCredits(t *testing.T) {
	dir := t.TempDir()
	writeStubConfig(t, dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/_balance", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"credits":      int64(1234),
			"last_updated": int64(1700000000),
			"source":       "cached",
		})
	}))
	defer srv.Close()
	require.NoError(t, writeDiscoveryFile(dir, srv.URL+"/"))

	out, _, err := runBalanceCmd(t, dir)
	require.NoError(t, err)
	assert.Contains(t, out, "1234")
	assert.Contains(t, out, "cached")
}

func TestBalanceCmd_JSONFlag_PrintsRawJSON(t *testing.T) {
	dir := t.TempDir()
	writeStubConfig(t, dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"credits":42,"source":"cached"}`+"\n")
	}))
	defer srv.Close()
	require.NoError(t, writeDiscoveryFile(dir, srv.URL+"/"))

	out, _, err := runBalanceCmd(t, dir, "--json")
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &decoded))
	assert.Equal(t, float64(42), decoded["credits"])
}

func TestBalanceCmd_SidecarDown_NonZeroExit(t *testing.T) {
	dir := t.TempDir()
	writeStubConfig(t, dir)

	_, errOut, err := runBalanceCmd(t, dir)
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(errOut+err.Error()), "sidecar")
}

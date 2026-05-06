package admission

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newAdminHTTPServer(t *testing.T, s *Subsystem) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	guard := func(next http.Handler) http.Handler {
		return BasicAuthGuard(next, func(token string) bool { return token == "test-token" })
	}
	s.RegisterMux(mux, guard)
	return httptest.NewServer(mux)
}

func adminReq(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	r, err := http.NewRequest(method, url, strings.NewReader(body))
	require.NoError(t, err)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(r)
	require.NoError(t, err)
	return resp
}

func TestAdmin_Status(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 0.5})

	srv := newAdminHTTPServer(t, s)
	defer srv.Close()

	resp := adminReq(t, "GET", srv.URL+"/admission/status", "test-token", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.InDelta(t, 0.5, out["pressure"], 1e-9)
	assert.InDelta(t, 10.0, out["supply_total_headroom"], 1e-9)
}

func TestAdmin_Status_RejectsMissingAuth(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	srv := newAdminHTTPServer(t, s)
	defer srv.Close()

	resp := adminReq(t, "GET", srv.URL+"/admission/status", "", "")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAdmin_Queue_ListsEntries(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 1.0})
	for i := 0; i < 3; i++ {
		s.Decide(makeIDi(i), nil, now)
	}
	srv := newAdminHTTPServer(t, s)
	defer srv.Close()

	resp := adminReq(t, "GET", srv.URL+"/admission/queue", "test-token", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out []map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Len(t, out, 3)
}

func TestAdmin_QueueDrain_WritesOverride(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlogPath := filepath.Join(dir, "admission.tlog")
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlogPath))
	s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 1.0})
	for i := 0; i < 3; i++ {
		s.Decide(makeIDi(i), nil, now)
	}
	srv := newAdminHTTPServer(t, s)
	defer srv.Close()

	resp := adminReq(t, "POST", srv.URL+"/admission/queue/drain", "test-token", `{"n":2}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, s.tlog.Close())

	recs, _, err := readTLogFile(tlogPath)
	require.NoError(t, err)
	var found bool
	for _, r := range recs {
		if r.Kind == TLogKindOperatorOverride {
			found = true
			break
		}
	}
	assert.True(t, found, "queue/drain emitted OPERATOR_OVERRIDE")
}

func TestAdmin_Consumer_404OnUnknown(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	srv := newAdminHTTPServer(t, s)
	defer srv.Close()

	resp := adminReq(t, "GET", srv.URL+"/admission/consumer/00000000000000000000000000000000000000000000000000000000000000ff",
		"test-token", "")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestAdmin_Snapshot_TriggersEmit(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(filepath.Join(dir, "admission.tlog")),
		WithSnapshotPrefix(filepath.Join(dir, "snap")))
	srv := newAdminHTTPServer(t, s)
	defer srv.Close()

	resp := adminReq(t, "POST", srv.URL+"/admission/snapshot", "test-token", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	files, _ := enumerateSnapshots(filepath.Join(dir, "snap"))
	assert.Len(t, files, 1)
}

func TestAdmin_PeersBlocklist_AddRemove(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(filepath.Join(t.TempDir(), "admission.tlog")))
	srv := newAdminHTTPServer(t, s)
	defer srv.Close()

	peer := "00000000000000000000000000000000000000000000000000000000000000aa"

	resp := adminReq(t, "POST", srv.URL+"/admission/peers/blocklist/"+peer, "test-token", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp = adminReq(t, "GET", srv.URL+"/admission/peers/blocklist", "test-token", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out []string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Contains(t, out, peer)

	resp = adminReq(t, "DELETE", srv.URL+"/admission/peers/blocklist/"+peer, "test-token", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp = adminReq(t, "GET", srv.URL+"/admission/peers/blocklist", "test-token", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out2 []string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out2))
	assert.NotContains(t, out2, peer)
}

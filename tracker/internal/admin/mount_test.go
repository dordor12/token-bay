package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSubsystemMounts_AreGuarded verifies broker and admission mount
// points are wrapped with the admin server's bearer-token guard. The
// subsystem handlers themselves don't need to know about auth — admin
// supplies the guard.
func TestSubsystemMounts_AreGuarded(t *testing.T) {
	brokerHits := 0
	brokerMux := http.NewServeMux()
	brokerMux.HandleFunc("GET /broker/inflight", func(w http.ResponseWriter, _ *http.Request) {
		brokerHits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	})

	admissionHits := 0
	admissionMount := func(mux *http.ServeMux, guard MuxGuard) {
		mux.Handle("GET /admission/status",
			guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				admissionHits++
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			})))
	}

	srv := newTestServer(t, func(d *Deps) {
		d.BrokerMux = brokerMux
		d.AdmissionMount = admissionMount
	})
	defer srv.Close()

	// Without auth: 401 from both subsystems.
	resp := adminReq(t, "GET", srv.URL+"/broker/inflight", "", "")
	resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Equal(t, 0, brokerHits)

	resp = adminReq(t, "GET", srv.URL+"/admission/status", "", "")
	resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Equal(t, 0, admissionHits)

	// With auth: pass-through.
	resp = adminReq(t, "GET", srv.URL+"/broker/inflight", "test-token", "")
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, brokerHits)

	resp = adminReq(t, "GET", srv.URL+"/admission/status", "test-token", "")
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, 1, admissionHits)
}

// TestSubsystemMounts_NilSafe ensures the mux still serves core routes
// when subsystem mounts are nil (early-boot scenarios, isolated tests).
func TestSubsystemMounts_NilSafe(t *testing.T) {
	srv, err := New(newTestDeps())
	require.NoError(t, err)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := adminReq(t, "GET", ts.URL+"/health", "test-token", "")
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	resp = adminReq(t, "GET", ts.URL+"/broker/inflight", "test-token", "")
	resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

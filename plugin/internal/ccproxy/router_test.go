package ccproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPassThroughRouter_ForwardsRequestAndResponse(t *testing.T) {
	// Mock upstream: echoes method+path and the Authorization header.
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"method":"`+r.Method+`","path":"`+r.URL.Path+`"}`)
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	router := &PassThroughRouter{UpstreamURL: u}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"x"}`))
	req.Header.Set("Authorization", "Bearer secret-token-xyz")
	rec := httptest.NewRecorder()

	router.Route(rec, req, nil)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"path":"/v1/messages"`)
	assert.Equal(t, "Bearer secret-token-xyz", gotAuth)
}

func TestNetworkRouter_Returns501_WithAnthropicErrorJSON(t *testing.T) {
	r := &NetworkRouter{}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	r.Route(rec, req, &EntryMetadata{})

	assert.Equal(t, http.StatusNotImplemented, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"type":"error"`)
	assert.Contains(t, body, `"not_implemented"`)
}

// Compile-time interface checks.
var (
	_ RequestRouter = (*PassThroughRouter)(nil)
	_ RequestRouter = (*NetworkRouter)(nil)
)

func TestNewPassThroughRouter_DefaultUpstreamIsAnthropic(t *testing.T) {
	r := NewPassThroughRouter()
	require.NotNil(t, r.UpstreamURL)
	assert.Equal(t, "https", r.UpstreamURL.Scheme)
	assert.Equal(t, "api.anthropic.com", r.UpstreamURL.Host)
}

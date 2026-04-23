package ccproxy

import (
	"net/http"
	"net/http/httputil"
	"net/url"
)

// RequestRouter handles a classified request. Server resolves session
// mode → router → Route.
type RequestRouter interface {
	Route(w http.ResponseWriter, r *http.Request, meta *EntryMetadata)
}

// PassThroughRouter forwards to Anthropic via httputil.ReverseProxy.
// Original headers (including Authorization) are copied byte-for-byte.
type PassThroughRouter struct {
	UpstreamURL *url.URL
	proxy       *httputil.ReverseProxy
}

// NewPassThroughRouter returns a router targeting api.anthropic.com.
func NewPassThroughRouter() *PassThroughRouter {
	u, _ := url.Parse("https://api.anthropic.com")
	return &PassThroughRouter{UpstreamURL: u}
}

// Route forwards r to the upstream and writes the response to w.
func (p *PassThroughRouter) Route(w http.ResponseWriter, r *http.Request, _ *EntryMetadata) {
	if p.proxy == nil {
		p.proxy = httputil.NewSingleHostReverseProxy(p.UpstreamURL)
	}
	// Rewrite Host so Anthropic's TLS cert matches.
	r.Host = p.UpstreamURL.Host
	p.proxy.ServeHTTP(w, r)
}

// NetworkRouter is the v1 stub. Real implementation lands in a later
// feature plan (ccproxy-network) once shared/proto envelopes and
// internal/tunnel exist.
type NetworkRouter struct{}

// Route returns a 501 with an Anthropic-style error payload so Claude
// Code surfaces the failure as a normal API error.
func (n *NetworkRouter) Route(w http.ResponseWriter, _ *http.Request, _ *EntryMetadata) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	_, _ = w.Write([]byte(`{"type":"error","error":{"type":"not_implemented",` +
		`"message":"Token-Bay network routing not yet implemented in this build (v1 stub)."}}`))
}

package ccproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	r.Host = p.UpstreamURL.Host
	p.proxy.ServeHTTP(w, r)
}

// PeerDialer opens a tunnel to the seeder identified by meta. Production
// wiring (network_dialer.go) injects a tunnelDialer; tests inject fakes.
type PeerDialer interface {
	Dial(ctx context.Context, meta *EntryMetadata) (PeerConn, error)
}

// PeerConn is the half-duplex consumer-side tunnel surface.
type PeerConn interface {
	// Send writes the consumer's request body once.
	Send(body []byte) error
	// Receive returns (statusOK, errMsg, body, err). On statusOK==true,
	// body streams the seeder's SSE bytes until EOF or peer-close. On
	// statusOK==false, errMsg holds the seeder's UTF-8 error text and
	// body is nil. err is non-nil only on transport failures.
	Receive(ctx context.Context) (statusOK bool, errMsg string, body io.Reader, err error)
	// Close tears down the underlying tunnel. Idempotent.
	Close() error
}

// ErrNoAssignment is returned by a PeerDialer when EntryMetadata lacks
// the seeder routing fields.
var ErrNoAssignment = errors.New("ccproxy: no seeder assignment in entry metadata")

// maxRequestBytes mirrors tunnel.Config.MaxRequestBytes to prevent a
// caller from accidentally building a body larger than the wire allows.
const maxRequestBytes = 1 << 20

// NetworkRouter forwards ModeNetwork sessions over a tunnel to a seeder.
// Dialer is required at first call to Route; tests use fakes, production
// wires a tunnelDialer (see network_dialer.go).
type NetworkRouter struct {
	Dialer PeerDialer
}

// Route implements the consumer-side network forward. Failure modes map
// to Anthropic-shaped JSON error bodies; success sets Content-Type:
// text/event-stream and io.Copies the seeder's bytes verbatim.
func (n *NetworkRouter) Route(w http.ResponseWriter, r *http.Request, meta *EntryMetadata) {
	if meta == nil {
		writeAnthropicError(w, http.StatusBadGateway, "network_unavailable", "Token-Bay session not in network mode")
		return
	}
	if n.Dialer == nil {
		writeAnthropicError(w, http.StatusBadGateway, "network_unavailable", "Token-Bay network dialer not configured")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request", "failed to read request body")
		return
	}
	if len(body) > maxRequestBytes {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request", "request body exceeds 1 MiB")
		return
	}

	conn, err := n.Dialer.Dial(r.Context(), meta)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "network_unavailable", sanitizeDialErr(err))
		return
	}
	defer conn.Close()

	if err := conn.Send(body); err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "network_unavailable", "tunnel send failed")
		return
	}

	ok, errMsg, rdr, err := conn.Receive(r.Context())
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "network_unavailable", "tunnel receive failed")
		return
	}
	if !ok {
		writeAnthropicError(w, http.StatusBadGateway, "network_unavailable", errMsg)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		nb, rerr := rdr.Read(buf)
		if nb > 0 {
			if _, werr := w.Write(buf[:nb]); werr != nil {
				return // client gone; nothing more to do
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			// io.EOF is the normal termination signal from the seeder.
			return
		}
	}
}

// writeAnthropicError sets a JSON error body in the shape Claude Code
// surfaces naturally. The handler MUST not have written any bytes yet
// (Content-Type / status are still mutable).
func writeAnthropicError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	payload := map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": msg,
		},
	}
	_ = json.NewEncoder(w).Encode(payload)
}

// sanitizeDialErr maps tunnel/quic error chains to short, PII-free
// strings safe to surface to the consumer's claude.
func sanitizeDialErr(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrNoAssignment) {
		return "no seeder assigned"
	}
	// Sentinel matching for tunnel.Err* lives in network_dialer.go's
	// adapter so the router stays import-free.
	return fmt.Sprintf("dial failed: %s", short(err.Error()))
}

func short(s string) string {
	const maxLen = 200
	if len(s) > maxLen {
		return s[:maxLen] + "…"
	}
	return s
}

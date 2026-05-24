package ccproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/token-bay/token-bay/plugin/internal/hooks"
)

// bufferedResponseWriter captures bytes written by hooks.Dispatcher.Handle
// so the caller can decide the HTTP status AFTER the dispatcher has chosen
// to write its EmptyResponse body. The dispatcher does not call WriteHeader
// — it's an io.Writer to it — so we only intercept Write.
type bufferedResponseWriter struct {
	http.ResponseWriter
	buf bytes.Buffer
}

func (b *bufferedResponseWriter) Write(p []byte) (int, error) {
	return b.buf.Write(p)
}

// hookRoutePrefix is the HTTP path under which the sidecar surfaces its
// hook-event ingestion endpoint. The hook subprocess (cmd-layer) POSTs the
// raw stdin JSON to <hookRoutePrefix>{EventName} and forwards the response
// body to its own stdout, satisfying the host Claude Code's hook contract.
//
// The leading underscore keeps the route namespace disjoint from Anthropic's
// SDK surface (/v1/*) — there is no Anthropic endpoint with a leading
// underscore. Explicit mux routes (see Server.Start) prevent fall-through
// to the upstream-forwarding handleAnthropic handler.
const hookRoutePrefix = "/_hooks/"

// statusRoutePath is the HTTP path the /token-bay status slash command
// fetches to render the supervisor's current state. Same underscore
// namespace as hookRoutePrefix; same explicit mux entry; same isolation
// from upstream Anthropic traffic.
const statusRoutePath = "/_status"

// WithHookSink injects the Sink the /_hooks/{event} HTTP handler routes
// parsed events into. The Sink is the integration seam between ccproxy's
// HTTP listener and the long-lived sidecar's consumerflow.Coordinator —
// the per-hook subprocess (token-bay-sidecar hooks ...) POSTs over loopback
// to deliver events to the in-process Coordinator.
//
// Nil-safe: if no sink is set, the handler still responds 200 with an
// EmptyResponse body so the host Claude Code's hook executor sees a clean
// reply. The Plugin CLAUDE.md hook contract — observation MUST never block
// the host turn — is enforced at this seam.
func WithHookSink(sink hooks.Sink) Option {
	return func(s *Server) { s.hookSink = sink }
}

// SetStatusProvider lets the cmd layer attach a snapshot-producer for the
// /_status endpoint. The supervisor (sidecar.App) supplies a closure that
// captures running-state + tracker-state + ccproxy URL; ccproxy renders the
// returned map as JSON. Nil provider yields a minimal {"running":false}
// reply — useful before the supervisor is wired.
func (s *Server) SetStatusProvider(fn func() map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusFn = fn
}

// handleHook implements POST /_hooks/{event}. Always 200 OK with an
// EmptyResponse body on the wire (hook observation must not block the host
// turn) UNLESS the event name is not in the known set or the payload
// cannot be parsed, in which case 400 lets the cmd-layer subprocess
// stderr-log a clear diagnostic. Even then, an EmptyResponse body is
// written so the host turn stays clean.
//
// Sink errors are NOT surfaced on the wire — the dispatcher returns them
// for logging, and the HTTP layer treats them as "still write 200, since
// the host's hook executor checks only the JSON output, not the status."
// The contract is documented on hooks.Dispatcher.Handle.
func (s *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	event := strings.TrimPrefix(r.URL.Path, hookRoutePrefix)
	if event == "" || strings.Contains(event, "/") {
		http.Error(w, "missing event name", http.StatusBadRequest)
		return
	}

	sink := s.hookSink
	if sink == nil {
		// No sink wired: degrade to the trivial NopSink. The host turn must
		// still see a clean EmptyResponse — never a 5xx or empty body.
		sink = &hooks.NopSink{}
	}
	d := &hooks.Dispatcher{Sink: sink}

	// The dispatcher writes the EmptyResponse body on every parsed path
	// (success OR sink-error). Use a buffering ResponseWriter so a
	// parse-failure / unknown-event error path can set HTTP 400 BEFORE any
	// body bytes flush — the host hook executor inspects the JSON output,
	// not the HTTP status, so the body is still written; the status is a
	// diagnostic signal to the hook subprocess for stderr logging.
	bw := &bufferedResponseWriter{ResponseWriter: w}
	err := d.Handle(r.Context(), event, r.Body, bw)

	status := http.StatusOK
	if err != nil && (errors.Is(err, hooks.ErrUnknownEvent) || isParseError(err)) {
		status = http.StatusBadRequest
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if bw.buf.Len() > 0 {
		_, _ = w.Write(bw.buf.Bytes())
	} else {
		// Defense in depth: if the dispatcher returned BEFORE writing
		// (parse failure is the typical case), still emit an EmptyResponse
		// so the host turn sees clean JSON.
		_ = hooks.EmptyResponse().Encode(w)
	}
}

// isParseError sniffs an error returned by hooks.Dispatcher.Handle for the
// "payload parse failed" case. The dispatcher wraps every parse failure
// with "hooks: parse <Event> payload:" / "hooks: expected hook_event_name="
// prefixes from events.go and ratelimit/stopfailure.go — we match on the
// prefix rather than allocating sentinel errors at every parse site.
func isParseError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "parse ") || strings.Contains(s, "expected hook_event_name")
}

// handleStatus implements GET /_status. Returns whatever the cmd-layer
// statusFn provides, defaulting to {"running":false} when no provider is
// wired (the supervisor sets one once App.Run starts the subsystems).
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	fn := s.statusFn
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if fn == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"running": false})
		return
	}
	_ = json.NewEncoder(w).Encode(fn())
}

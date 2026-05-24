package ccproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/token-bay/token-bay/plugin/internal/hooks"
)

// Server is the ccproxy HTTP listener.
type Server struct {
	Addr        string
	Store       *SessionModeStore
	PassThrough RequestRouter
	Network     RequestRouter
	Started     time.Time

	mu           sync.Mutex
	listener     net.Listener
	srv          *http.Server
	resolvedAddr string

	// hookSink, when non-nil, is dispatched to from /_hooks/{event}.
	hookSink hooks.Sink

	// statusProvider supplies the snapshot rendered by GET /_status.
	statusProvider func() any
	// balanceProvider supplies the snapshot rendered by GET /_balance.
	balanceProvider func() any
}

// Option is a functional configuration option.
type Option func(*Server)

// WithAddr overrides the default bind address.
func WithAddr(addr string) Option {
	return func(s *Server) { s.Addr = addr }
}

// WithPassThroughRouter injects a router (primarily for tests).
func WithPassThroughRouter(r RequestRouter) Option {
	return func(s *Server) { s.PassThrough = r }
}

// WithNetworkRouter injects a router (primarily for tests).
func WithNetworkRouter(r RequestRouter) Option {
	return func(s *Server) { s.Network = r }
}

// WithSessionStore overrides the default SessionModeStore.
func WithSessionStore(store *SessionModeStore) Option {
	return func(s *Server) { s.Store = store }
}

// WithHookSink injects the hook Sink that handles POST /_hooks/{event}.
// When nil, /_hooks/* still returns 200 with hooks.EmptyResponse so the
// host Claude Code turn never blocks on a missing supervisor wiring.
func WithHookSink(sink hooks.Sink) Option {
	return func(s *Server) { s.hookSink = sink }
}

// SetStatusProvider installs a closure that supplies the JSON body for
// GET /_status. Safe to call before or after Start. When unset, /_status
// returns 503.
func (s *Server) SetStatusProvider(fn func() any) {
	s.mu.Lock()
	s.statusProvider = fn
	s.mu.Unlock()
}

// SetBalanceProvider installs a closure that supplies the JSON body for
// GET /_balance. Safe to call before or after Start. When unset,
// /_balance returns 503.
func (s *Server) SetBalanceProvider(fn func() any) {
	s.mu.Lock()
	s.balanceProvider = fn
	s.mu.Unlock()
}

// New constructs a Server with defaults and applies the given options.
func New(opts ...Option) *Server {
	s := &Server{
		Addr:        "127.0.0.1:0",
		Store:       NewSessionModeStore(),
		PassThrough: NewPassThroughRouter(),
		Network:     &NetworkRouter{Dialer: NewTunnelDialer()},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Start binds the listener and begins serving. Returns once ready.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("ccproxy: listen %s: %w", s.Addr, err)
	}
	s.listener = ln
	s.resolvedAddr = ln.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("/token-bay/health", s.handleHealth)
	// Hooks IPC + introspection endpoints. Mounted as explicit routes so
	// they never fall through to /v1/messages — plugin CLAUDE.md rule #3
	// forbids touching upstream bytes on a hook payload.
	mux.HandleFunc("/_hooks/", s.handleHooks)
	mux.HandleFunc("/_status", s.handleStatus)
	mux.HandleFunc("/_balance", s.handleBalance)
	mux.HandleFunc("/", s.handleAnthropic)

	s.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	s.Started = time.Now()

	go func() { _ = s.srv.Serve(ln) }()

	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()

	return nil
}

// URL returns the base URL including the resolved port.
func (s *Server) URL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resolvedAddr == "" {
		return ""
	}
	return "http://" + s.resolvedAddr + "/"
}

// Close shuts the server down.
func (s *Server) Close() error {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

// handleAnthropic routes Anthropic-API-shaped requests (anything not
// /token-bay/*). Resolves the session mode and delegates to the router.
func (s *Server) handleAnthropic(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get(sessionIDHeader)
	mode, meta := s.Store.GetMode(sessionID)
	switch mode {
	case ModeNetwork:
		s.Network.Route(w, r, meta)
	default:
		s.PassThrough.Route(w, r, nil)
	}
}

// handleHooks dispatches POST /_hooks/{event} to the configured Sink.
//
// Contract (plugin spec §2.1 + plugin CLAUDE.md rule #3):
//   - Always returns 200 + hooks.EmptyResponse on Sink/parse/unknown
//     errors. The host Claude Code turn must never block on a sidecar
//     that is unhealthy or missing.
//   - Never falls through to /v1/messages (this route is mounted as an
//     explicit prefix on the ServeMux).
//   - When no Sink is wired, still returns EmptyResponse so the hook
//     subprocess sees a clean reply.
func (s *Server) handleHooks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		// Hooks are always POSTs. A non-POST is a misconfiguration but
		// must not surface a blocking error to the host turn either.
		_ = hooks.EmptyResponse().Encode(w)
		return
	}

	event := strings.TrimPrefix(r.URL.Path, "/_hooks/")
	if event == "" || s.hookSink == nil {
		_ = hooks.EmptyResponse().Encode(w)
		return
	}

	// Dispatcher.Handle writes hooks.EmptyResponse on Sink-error and
	// success paths, but writes nothing on parse-error / unknown-event
	// paths. Wrap w in a counter so we can append EmptyResponse to the
	// no-write paths and keep the host-turn contract (always 200 + {}).
	cw := &countingWriter{ResponseWriter: w}
	d := &hooks.Dispatcher{Sink: s.hookSink}
	if err := d.Handle(r.Context(), event, r.Body, cw); err != nil && cw.n == 0 {
		_ = hooks.EmptyResponse().Encode(cw)
	}
}

// countingWriter wraps an http.ResponseWriter so handleHooks can detect
// whether the dispatcher wrote a body on its no-write paths.
type countingWriter struct {
	http.ResponseWriter
	n int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += n
	return n, err
}

// handleStatus returns the snapshot from the injected statusProvider.
// 503 when no provider has been installed.
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	fn := s.statusProvider
	s.mu.Unlock()
	if fn == nil {
		http.Error(w, `{"error":"status provider not installed"}`, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(fn())
}

// handleBalance returns the snapshot from the injected balanceProvider.
// 503 when no provider has been installed.
func (s *Server) handleBalance(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	fn := s.balanceProvider
	s.mu.Unlock()
	if fn == nil {
		http.Error(w, `{"error":"balance provider not installed"}`, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(fn())
}

// handleHealth returns a small JSON status blob. Used by the runtime
// compatibility probe (future feature) and for operator debugging.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	uptime := time.Since(s.Started).Seconds()
	payload := map[string]any{
		"status":     "ok",
		"addr":       s.resolvedAddr,
		"uptime_sec": uptime,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

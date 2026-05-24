package ccproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
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

	// hookSink is the (optional) destination for /_hooks/{event} POSTs.
	// Wired by the sidecar supervisor via WithHookSink; remains nil in
	// tests that never exercise the IPC seam. Read without locking from
	// handleHook — assignment happens exclusively during option-apply,
	// before Start, so concurrent mutation is structurally impossible.
	hookSink hooks.Sink

	// statusFn renders the /_status endpoint's body. Wired post-Start by
	// the supervisor (the URL is only known after the OS picks a port).
	// Guarded by mu because SetStatusProvider may race with handleStatus.
	statusFn func() map[string]any
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
	// Plugin CLAUDE.md rule #3: the /_hooks/* and /_status paths are SEPARATE
	// from upstream Anthropic proxying — they must be explicit mux entries
	// that do NOT fall through to handleAnthropic (which would forward the
	// hook event body to api.anthropic.com). The leading underscore keeps
	// the namespace disjoint from anything the Anthropic SDK ships.
	mux.HandleFunc(hookRoutePrefix, s.handleHook)
	mux.HandleFunc(statusRoutePath, s.handleStatus)
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

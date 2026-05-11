package admin

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/registry"
)

// ErrAlreadyRunning is returned by Run when called a second time.
var ErrAlreadyRunning = errors.New("admin: already running")

// PeerCounter reports the count of currently-connected QUIC peers.
// *server.Server satisfies this; the indirection keeps admin tests free of
// the QUIC server.
type PeerCounter interface {
	PeerCount() int
}

// LedgerView is the read surface admin needs from the ledger subsystem.
// *ledger.Ledger satisfies this.
type LedgerView interface {
	Tip(ctx context.Context) (uint64, []byte, bool, error)
	SignedBalance(ctx context.Context, identityID []byte) (*tbproto.SignedBalanceSnapshot, error)
}

// RegistryView is the read surface admin needs from the seeder registry.
// *registry.Registry satisfies this.
type RegistryView interface {
	Get(id ids.IdentityID) (registry.SeederRecord, bool)
}

// AdmissionMount lets the admission subsystem register its routes on a
// mux behind the supplied guard. The signature mirrors
// admission.Subsystem.RegisterMux; cmd/run_cmd writes a one-line adapter
// that converts admin.MuxGuard to admission.MuxGuard.
type AdmissionMount func(mux *http.ServeMux, guard MuxGuard)

// Deps is everything the admin Server needs from the outside world.
// cmd/run_cmd builds this from its already-constructed subsystems and
// passes it to New. Required fields are documented per-field; a missing
// required field causes New to return an error.
type Deps struct {
	Config         *config.Config // required (uses Admin.ListenAddr, Ledger.MerkleRootIntervalMin)
	Logger         zerolog.Logger
	Now            func() time.Time // defaults to time.Now
	Version        string           // build version, surfaced by /health
	TokenValidator TokenValidator   // required; nil rejects every request

	PeerCounter PeerCounter  // required
	Registry    RegistryView // required
	Ledger      LedgerView   // required

	// Federation is the operator-facing view of the federation
	// subsystem. Optional: when nil, GET /peers reports federation as
	// disabled and POST /peers/add|remove return 501.
	Federation FederationView

	// Subsystem mounts. Either may be nil; routes simply won't be mounted.
	BrokerMux      http.Handler
	AdmissionMount AdmissionMount

	// TriggerMaintenance is invoked by POST /maintenance. cmd/run_cmd
	// wires it to cancel the main signal context; that path drains the
	// QUIC server and then this admin server. Required.
	TriggerMaintenance func()
}

// Server is the admin HTTP server. Single-call: a second Run returns
// ErrAlreadyRunning. Shutdown is idempotent and safe before Run.
type Server struct {
	deps    Deps
	handler http.Handler

	mu         sync.Mutex
	running    bool
	stopped    bool
	listenAddr string
	httpSrv    *http.Server
	listener   net.Listener
}

// New validates Deps and returns a Server ready to Run.
func New(d Deps) (*Server, error) {
	if d.Config == nil {
		return nil, errors.New("admin: Config required")
	}
	if d.Config.Admin.ListenAddr == "" {
		return nil, errors.New("admin: Config.Admin.ListenAddr required")
	}
	if d.TokenValidator == nil {
		return nil, errors.New("admin: TokenValidator required")
	}
	if d.PeerCounter == nil {
		return nil, errors.New("admin: PeerCounter required")
	}
	if d.Registry == nil {
		return nil, errors.New("admin: Registry required")
	}
	if d.Ledger == nil {
		return nil, errors.New("admin: Ledger required")
	}
	if d.TriggerMaintenance == nil {
		return nil, errors.New("admin: TriggerMaintenance required")
	}
	if d.Now == nil {
		d.Now = time.Now
	}

	s := &Server{deps: d}
	s.handler = s.buildMux()
	return s, nil
}

// ListenAddr returns the bound listener address, or "" before Run binds
// or after Shutdown closes the listener. Tests dialing :0 use this.
func (s *Server) ListenAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listenAddr
}

// Handler returns the assembled http.Handler. Exposed for tests that
// drive the server with httptest instead of a real listener.
func (s *Server) Handler() http.Handler { return s.handler }

// Run binds the listener and serves until ctx cancels or Shutdown is
// called. Returns nil on graceful shutdown, error on listener failure.
func (s *Server) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return ErrAlreadyRunning
	}
	s.running = true
	s.mu.Unlock()

	ln, err := net.Listen("tcp", s.deps.Config.Admin.ListenAddr)
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Handler:      s.handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	s.mu.Lock()
	s.listenAddr = ln.Addr().String()
	s.httpSrv = httpSrv
	s.listener = ln
	s.mu.Unlock()

	s.deps.Logger.Info().Str("addr", ln.Addr().String()).Msg("admin: listening")

	go func() {
		<-ctx.Done()
		// Best-effort drain on ctx cancel; Shutdown(ctx) handles the
		// proper grace window when the operator calls it directly.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	err = httpSrv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown gracefully drains in-flight requests up to ctx, then force-
// closes. Idempotent: a second Shutdown returns nil. Calling Shutdown
// before Run returns nil.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.running || s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	httpSrv := s.httpSrv
	s.mu.Unlock()
	if httpSrv == nil {
		return nil
	}
	return httpSrv.Shutdown(ctx)
}

// buildMux assembles the routing tree. All routes pass through the
// bearer-token guard.
func (s *Server) buildMux() http.Handler {
	guard := bearerGuard(s.deps.TokenValidator)
	mux := http.NewServeMux()

	// Core spec §8 routes.
	mux.Handle("GET /health", guard(http.HandlerFunc(s.handleHealth)))
	mux.Handle("GET /stats", guard(http.HandlerFunc(s.handleStats)))
	mux.Handle("GET /peers", guard(http.HandlerFunc(s.handlePeers)))
	mux.Handle("POST /peers/add", guard(http.HandlerFunc(s.handlePeersAdd)))
	mux.Handle("POST /peers/remove", guard(http.HandlerFunc(s.handlePeersRemove)))
	mux.Handle("GET /identity/{id}", guard(http.HandlerFunc(s.handleIdentity)))
	mux.Handle("POST /maintenance", guard(http.HandlerFunc(s.handleMaintenance)))

	// Subsystem mounts. Each subsystem owns its own route paths
	// (broker mounts /broker/*, admission mounts /admission/*).
	if s.deps.BrokerMux != nil {
		mux.Handle("/broker/", guard(s.deps.BrokerMux))
	}
	if s.deps.AdmissionMount != nil {
		s.deps.AdmissionMount(mux, guard)
	}

	return mux
}

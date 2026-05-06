package server

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/api"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
	"github.com/token-bay/token-bay/tracker/internal/registry"
	"github.com/token-bay/token-bay/tracker/internal/stunturn"
)

// ErrAlreadyRunning is returned by Run when called a second time.
var ErrAlreadyRunning = errors.New("server: already running")

// Dispatcher is what server needs from api. *api.Router satisfies it.
// Declared here as the consumer (Go's "accept interfaces" idiom).
type Dispatcher interface {
	Dispatch(ctx context.Context, rc *api.RequestCtx, req *tbproto.RpcRequest) *tbproto.RpcResponse
	PushAPI() api.PushAPI
}

// Deps is everything Server needs from the outside world. cmd/run_cmd
// builds this and passes it to New.
type Deps struct {
	Config   *config.Config
	Logger   zerolog.Logger
	Now      func() time.Time
	Registry *registry.Registry
	Ledger   *ledger.Ledger
	StunTurn *stunturn.Allocator
	Reflect  func(addr netip.AddrPort) netip.AddrPort
	API      Dispatcher
}

// Server owns the QUIC listener, mTLS termination, and the per-peer
// connection lifecycle. Single-call: a second Run returns
// ErrAlreadyRunning. Shutdown is idempotent.
type Server struct {
	deps Deps

	mu          sync.Mutex
	running     bool
	stopped     bool
	connsByPeer map[ids.IdentityID]*Connection

	serverCtx    context.Context
	serverCancel context.CancelFunc

	wg sync.WaitGroup // counts in-flight RPC dispatch goroutines
}

// New validates Deps and returns a Server. Run binds the listener.
func New(d Deps) (*Server, error) {
	if d.Config == nil {
		return nil, errors.New("server: Config required")
	}
	if d.API == nil {
		return nil, errors.New("server: API dispatcher required")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Server{
		deps:        d,
		connsByPeer: map[ids.IdentityID]*Connection{},
	}, nil
}

// PeerCount returns the number of currently-connected peers.
func (s *Server) PeerCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.connsByPeer)
}

// Run blocks until ctx is cancelled or a fatal listener error occurs.
// Phase 5 wires the accept loop; for now Run returns
// ErrNotImplementedRun so cmd/run_cmd compiles against the real type.
func (s *Server) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return ErrAlreadyRunning
	}
	s.running = true
	s.serverCtx, s.serverCancel = context.WithCancel(ctx)
	s.mu.Unlock()
	defer s.serverCancel()

	// Wired in Phase 5 (Task 17).
	return errors.New("server: Run not yet implemented (Phase 5)")
}

// Shutdown gracefully drains. Wired in Phase 9.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	s.mu.Unlock()
	return errors.New("server: Shutdown not yet implemented (Phase 9)")
}

// PushOfferTo enqueues an OfferPush. Wired in Phase 8.
func (s *Server) PushOfferTo(_ ids.IdentityID, _ *tbproto.OfferPush) (<-chan *tbproto.OfferDecision, bool) {
	return nil, false
}

// PushSettlementTo mirrors PushOfferTo. Wired in Phase 8.
func (s *Server) PushSettlementTo(_ ids.IdentityID, _ *tbproto.SettlementPush) (<-chan *tbproto.SettleAck, bool) {
	return nil, false
}

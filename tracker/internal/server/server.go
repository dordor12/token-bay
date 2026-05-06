package server

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	quicgo "github.com/quic-go/quic-go"
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
	listenAddr  string // populated when Run binds the listener; "" until then
	connsByPeer map[ids.IdentityID]*Connection

	serverCtx    context.Context
	serverCancel context.CancelFunc

	wg sync.WaitGroup // counts in-flight RPC dispatch goroutines
}

// ListenAddr returns the bound listener address, or "" before Run binds
// or after Shutdown closes the listener. Used by tests dialing :0.
func (s *Server) ListenAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listenAddr
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
// It is the long-lived entry point for the QUIC server.
//
// Lifecycle:
//   - bind listener
//   - for each accepted QUIC conn: spawn serveConn (heartbeat + RPC streams)
//   - return when serverCtx cancels (Shutdown) or listener fails
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

	keyBytes, err := os.ReadFile(s.deps.Config.Server.IdentityKeyPath)
	if err != nil {
		return fmt.Errorf("server: read identity key: %w", err)
	}
	cert, err := ServerCertFromIdentity(ed25519.PrivateKey(keyBytes))
	if err != nil {
		return err
	}

	l, err := StartListener(&s.deps.Config.Server, cert)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.listenAddr = l.Addr().String()
	s.mu.Unlock()
	s.deps.Logger.Info().Str("addr", l.Addr().String()).Msg("server: listening")

	// Close the listener when Shutdown cancels serverCtx.
	go func() {
		<-s.serverCtx.Done()
		_ = l.Close()
	}()

	for {
		conn, err := l.Accept(s.serverCtx)
		if err != nil {
			if s.serverCtx.Err() != nil {
				return nil
			}
			return fmt.Errorf("server: accept: %w", err)
		}
		go s.serveConn(conn)
	}
}

// serveConn runs the per-peer state machine: extract IdentityID from
// mTLS, register the *Connection, accept the heartbeat stream, accept
// RPC streams (Phase 7), tear down on disconnect.
func (s *Server) serveConn(qc *quicgo.Conn) {
	state := qc.ConnectionState().TLS
	if len(state.PeerCertificates) != 1 {
		_ = qc.CloseWithError(0, "missing peer cert")
		return
	}
	peerID, err := SPKIToIdentityID(state.PeerCertificates[0])
	if err != nil {
		_ = qc.CloseWithError(0, "bad SPKI")
		return
	}
	udp, ok := qc.RemoteAddr().(*net.UDPAddr)
	if !ok {
		_ = qc.CloseWithError(0, "non-UDP remote")
		return
	}
	addr := udp.AddrPort()

	c := newConnection(s.serverCtx, qc, peerID, addr)
	s.mu.Lock()
	s.connsByPeer[peerID] = c
	s.mu.Unlock()
	s.deps.Logger.Info().Hex("peer", peerID[:]).Str("addr", addr.String()).
		Msg("server: peer connected")

	defer func() {
		s.mu.Lock()
		delete(s.connsByPeer, peerID)
		s.mu.Unlock()
		c.Close()
		s.deps.Logger.Info().Hex("peer", peerID[:]).Msg("server: peer disconnected")
	}()

	// First client-initiated stream → heartbeat. Mirror of the merged
	// plugin client behavior (heartbeat opens before any RPC).
	hbStream, err := qc.AcceptStream(s.serverCtx)
	if err != nil {
		return
	}
	c.streamSeq.Add(1)
	go runHeartbeat(s.serverCtx, hbStream, peerID, s.deps.Registry,
		s.deps.Now, s.deps.Config.Server.MaxFrameSize, s.deps.Logger)

	// Subsequent streams = RPC. Phase 7 wires handleRPCStream.
	for {
		stream, err := qc.AcceptStream(s.serverCtx)
		if err != nil {
			return
		}
		c.streamSeq.Add(1)
		go s.handleRPCStream(s.serverCtx, stream, c)
	}
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

// PushOfferTo and PushSettlementTo are defined in push_offers.go and
// push_settlements.go (Phase 8). Server holds the dispatch state; the
// push files own the per-stream wire dance.

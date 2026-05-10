package federation

import (
	"context"
	"errors"
	"sync"

	fed "github.com/token-bay/token-bay/shared/federation"
)

// Peer wraps a steady-state PeerConn with a recvLoop goroutine. The
// caller-supplied dispatch callback runs synchronously on the recv
// goroutine; it MUST NOT block, and MUST NOT call back into the Peer.
type Peer struct {
	conn     PeerConn
	dispatch func(*fed.Envelope)

	mu        sync.Mutex // guards cancel
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	startOnce sync.Once
	stopOnce  sync.Once
}

// NewPeerForTest is a thin constructor exposed only because tests need to
// poke a recv-only Peer without a registry. Production code uses Open's
// internal wiring.
func NewPeerForTest(conn PeerConn, dispatch func(*fed.Envelope)) *Peer {
	return &Peer{conn: conn, dispatch: dispatch}
}

// Start launches the background recvLoop tied to parent's lifetime.
// Idempotent — repeat calls are no-ops, preventing goroutine leaks if a
// caller accidentally re-Starts an already-running Peer.
func (p *Peer) Start(parent context.Context) {
	p.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(parent)
		p.mu.Lock()
		p.cancel = cancel
		p.mu.Unlock()
		p.wg.Add(1)
		go p.recvLoop(ctx)
	})
}

func (p *Peer) recvLoop(ctx context.Context) {
	defer p.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		frame, err := p.conn.Recv(ctx)
		if err != nil {
			return
		}
		env, err := UnmarshalFrame(frame)
		if err != nil {
			continue
		}
		p.dispatch(env)
	}
}

// Send is a thin wrapper that surfaces ErrPeerClosed cleanly.
func (p *Peer) Send(ctx context.Context, frame []byte) error {
	if err := p.conn.Send(ctx, frame); err != nil {
		if errors.Is(err, ErrPeerClosed) {
			return ErrPeerClosed
		}
		return err
	}
	return nil
}

// Stop cancels the recvLoop, closes the underlying connection, and
// waits for the goroutine to exit. Safe to call more than once.
func (p *Peer) Stop() {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		cancel := p.cancel
		p.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		_ = p.conn.Close()
		p.wg.Wait()
	})
}

// Wait blocks until the recvLoop exits (Stop has been called or the
// transport's Recv returned a terminal error). Used by the Dialer to
// observe connection drops and re-enter the redial loop. Idempotent.
func (p *Peer) Wait() { p.wg.Wait() }

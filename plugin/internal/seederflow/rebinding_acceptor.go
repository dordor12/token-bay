package seederflow

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/netip"
	"sync"
)

// TunnelListener is a single bound tunnel-listener-equivalent surface.
// Production wraps *tunnel.Listener via a thin adapter; tests provide a
// fake. A TunnelListener is bound to one (seederPriv, consumerPub) pair
// at construction time; reuse with a different pair requires creating a
// fresh listener via TunnelListenerFactory.
type TunnelListener interface {
	Accept(ctx context.Context) (TunnelConn, error)
	LocalAddr() netip.AddrPort
	Close() error
}

// TunnelListenerFactory creates a fresh TunnelListener bound to the
// given per-session keys. The seeder's private key is shipped as the
// listener's leaf cert; the consumer's pubkey is pinned via the
// listener's TLS verify callback. RebindingAcceptor calls the factory
// once per Bind. Returning an error fails the bind closed.
type TunnelListenerFactory func(seederPriv ed25519.PrivateKey, consumerPub ed25519.PublicKey) (TunnelListener, error)

// ErrAcceptorClosed is returned by Bind or Accept after Close.
var ErrAcceptorClosed = errors.New("seederflow: acceptor closed")

// RebindingAcceptor is a TunnelAcceptor that swaps its underlying
// TunnelListener every time Bind is invoked. The previous listener is
// closed before the new one is installed, so at most one consumer
// tunnel is in flight at a time per seeder (single-flight v1, matching
// the coordinator's reservation model — see run.go peekReservation).
//
// Accept blocks until Bind has installed a listener (or until ctx is
// canceled). When Bind supersedes the active listener, an Accept
// blocked on the old listener observes Close as a fresh-listener error
// and the cmd-layer accept loop picks up the new listener on its next
// iteration (see run.go acceptLoop's debug-log + backoff).
type RebindingAcceptor struct {
	factory TunnelListenerFactory

	mu     sync.Mutex
	cur    TunnelListener
	bound  chan struct{} // signaled (closed + replaced) on each Bind
	closed bool
}

// NewRebindingAcceptor constructs an acceptor that produces fresh
// TunnelListeners via factory. factory is required.
func NewRebindingAcceptor(factory TunnelListenerFactory) (*RebindingAcceptor, error) {
	if factory == nil {
		return nil, fmt.Errorf("%w: TunnelListenerFactory required", ErrInvalidConfig)
	}
	return &RebindingAcceptor{
		factory: factory,
		bound:   make(chan struct{}),
	}, nil
}

// Bind closes any prior listener and installs a fresh one bound to
// (seederPriv, consumerPub).
func (a *RebindingAcceptor) Bind(seederPriv ed25519.PrivateKey, consumerPub ed25519.PublicKey) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return ErrAcceptorClosed
	}
	prev := a.cur
	a.mu.Unlock()

	// Build the new listener outside the lock — factory may do I/O.
	next, err := a.factory(seederPriv, consumerPub)
	if err != nil {
		return fmt.Errorf("seederflow: rebinding listener: %w", err)
	}

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		_ = next.Close()
		return ErrAcceptorClosed
	}
	a.cur = next
	prevBound := a.bound
	a.bound = make(chan struct{})
	a.mu.Unlock()

	// Wake any Accept waiting on the previous binding.
	close(prevBound)
	if prev != nil {
		_ = prev.Close()
	}
	return nil
}

// Accept returns the next inbound TunnelConn from the currently-bound
// listener. If no listener is bound, Accept blocks until Bind is
// invoked or ctx is canceled.
func (a *RebindingAcceptor) Accept(ctx context.Context) (TunnelConn, error) {
	for {
		a.mu.Lock()
		if a.closed {
			a.mu.Unlock()
			return nil, ErrAcceptorClosed
		}
		cur := a.cur
		bound := a.bound
		a.mu.Unlock()

		if cur == nil {
			select {
			case <-bound:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return cur.Accept(ctx)
	}
}

// LocalAddr returns the active listener's bound address, or the zero
// AddrPort if no listener is bound.
func (a *RebindingAcceptor) LocalAddr() netip.AddrPort {
	a.mu.Lock()
	cur := a.cur
	a.mu.Unlock()
	if cur == nil {
		return netip.AddrPort{}
	}
	return cur.LocalAddr()
}

// Close releases the active listener. Idempotent.
func (a *RebindingAcceptor) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	cur := a.cur
	a.cur = nil
	bound := a.bound
	a.mu.Unlock()

	close(bound)
	if cur != nil {
		return cur.Close()
	}
	return nil
}

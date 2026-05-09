package federation

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
)

// InprocHub routes Dial calls between InprocTransports registered under a
// string addr. It exists so a test can wire two trackers without sockets.
type InprocHub struct {
	mu        sync.Mutex
	listeners map[string]*InprocTransport
}

func NewInprocHub() *InprocHub {
	return &InprocHub{listeners: make(map[string]*InprocTransport)}
}

// InprocTransport implements Transport in-process via channel-paired
// PeerConns. Safe for concurrent use. Construction registers the
// listener into the hub eagerly so a Dial racing the goroutine that
// runs Listen() always sees the addr — the accept callback is itself
// posted to a buffered channel by Listen and returned to the channel
// after each Dial uses it (single-listener-many-dials invariant).
type InprocTransport struct {
	hub    *InprocHub
	addr   string
	pub    ed25519.PublicKey
	priv   ed25519.PrivateKey
	accept chan func(PeerConn)

	mu     sync.Mutex
	closed bool
	conns  []*inprocConn
}

func NewInprocTransport(hub *InprocHub, addr string, pub ed25519.PublicKey, priv ed25519.PrivateKey) *InprocTransport {
	t := &InprocTransport{hub: hub, addr: addr, pub: pub, priv: priv, accept: make(chan func(PeerConn), 1)}
	hub.mu.Lock()
	hub.listeners[addr] = t
	hub.mu.Unlock()
	return t
}

func (t *InprocTransport) Listen(ctx context.Context, accept func(PeerConn)) error {
	// Single-listener invariant: if a callback is already pending, swap it.
	select {
	case <-t.accept:
	default:
	}
	t.accept <- accept
	<-ctx.Done()
	return ctx.Err()
}

func (t *InprocTransport) Dial(ctx context.Context, addr string, expectedPeer ed25519.PublicKey) (PeerConn, error) {
	// Lock order: srv.mu then t.mu (dialer side). Never invert.
	t.hub.mu.Lock()
	srv, ok := t.hub.listeners[addr]
	t.hub.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: addr %q has no listener", ErrHandshakeFailed, addr)
	}
	if !ed25519.PublicKey(srv.pub).Equal(expectedPeer) {
		return nil, fmt.Errorf("%w: addr %q peer pubkey mismatch", ErrHandshakeFailed, addr)
	}

	// Wait for the listener to post its accept callback.
	var fn func(PeerConn)
	select {
	case fn = <-srv.accept:
		srv.accept <- fn // put it back for the next dial
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	srv.mu.Lock()
	if srv.closed {
		srv.mu.Unlock()
		return nil, fmt.Errorf("%w: %q closed", ErrHandshakeFailed, addr)
	}
	pair := newInprocPair(t.pub, srv.pub)
	srv.conns = append(srv.conns, pair.right)
	t.mu.Lock()
	t.conns = append(t.conns, pair.left)
	t.mu.Unlock()
	srv.mu.Unlock()
	go fn(pair.right)
	return pair.left, nil
}

func (t *InprocTransport) Close() error {
	t.hub.mu.Lock()
	delete(t.hub.listeners, t.addr)
	t.hub.mu.Unlock()
	t.mu.Lock()
	t.closed = true
	conns := t.conns
	t.conns = nil
	t.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	return nil
}

// inprocConn is one half of a paired connection. Send on left lands on
// right.recv, and vice versa.
//
// Closure is signalled via a shared pairDone channel (one per pair) that
// is closed once by the first side that calls Close(). Both sides select
// on it so either Close() immediately unblocks all pending Send/Recv on
// both ends — avoiding the race between a concurrent Close() and Send().
type inprocConn struct {
	myPub     ed25519.PublicKey
	remotePub ed25519.PublicKey
	addr      string
	send      chan []byte   // outbound; the *peer* reads this
	recv      chan []byte   // inbound
	pairDone  chan struct{} // shared with peer; closed once by first Close()

	mu     sync.Mutex
	closed bool
	once   *sync.Once // shared with peer to close pairDone exactly once
}

type inprocPair struct {
	left, right *inprocConn
}

func newInprocPair(localPub, remotePub ed25519.PublicKey) *inprocPair {
	a := make(chan []byte, 16)
	bb := make(chan []byte, 16)
	done := make(chan struct{})
	once := &sync.Once{}
	left := &inprocConn{myPub: localPub, remotePub: remotePub, addr: "inproc-left", send: a, recv: bb, pairDone: done, once: once}
	right := &inprocConn{myPub: remotePub, remotePub: localPub, addr: "inproc-right", send: bb, recv: a, pairDone: done, once: once}
	return &inprocPair{left: left, right: right}
}

func (c *inprocConn) Send(ctx context.Context, frame []byte) error {
	if len(frame) > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	// Fast-path: if this side is already closed, return immediately.
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrPeerClosed
	}
	select {
	case c.send <- frame:
		return nil
	case <-c.pairDone:
		return ErrPeerClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *inprocConn) Recv(ctx context.Context) ([]byte, error) {
	select {
	case f, ok := <-c.recv:
		if !ok {
			return nil, ErrPeerClosed
		}
		return f, nil
	case <-c.pairDone:
		return nil, ErrPeerClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *inprocConn) RemoteAddr() string           { return c.addr }
func (c *inprocConn) RemotePub() ed25519.PublicKey { return c.remotePub }

func (c *inprocConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.once.Do(func() { close(c.pairDone) })
	return nil
}

var (
	_ Transport = (*InprocTransport)(nil)
	_ PeerConn  = (*inprocConn)(nil)
	_           = errors.New // keep import even if all errors are sentinels
)

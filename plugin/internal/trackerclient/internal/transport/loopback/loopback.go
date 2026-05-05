// Package loopback is an in-memory Transport for tests. Conns are paired
// in-process; OpenStreamSync on one side surfaces as AcceptStream on the
// other. There is no TLS — identity is asserted via a side-channel.
package loopback

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
	"github.com/token-bay/token-bay/shared/ids"
)

// Pair returns a (clientConn, serverConn) pair. clientConn.Dial-style
// behaviour is to return clientConn directly; tests call New(serverConn)
// to drive the server side in a goroutine.
func Pair(clientID, serverID ids.IdentityID) (*Conn, *Conn) {
	clientToServer := make(chan *streamPair, 16)
	serverToClient := make(chan *streamPair, 16)
	cli := &Conn{
		peerID:  serverID,
		opens:   clientToServer,
		accepts: serverToClient,
		done:    make(chan struct{}),
	}
	srv := &Conn{
		peerID:  clientID,
		opens:   serverToClient,
		accepts: clientToServer,
		done:    make(chan struct{}),
	}
	cli.peer = srv
	srv.peer = cli
	return cli, srv
}

// Driver implements transport.Transport for tests; ep.Addr selects which
// pre-registered server Conn to attach to.
type Driver struct {
	mu      sync.Mutex
	targets map[string]*Conn
}

func NewDriver() *Driver { return &Driver{targets: map[string]*Conn{}} }

// Listen registers a server Conn under addr. Tests typically call Pair,
// register the server side here, and dial via the same Driver.
func (d *Driver) Listen(addr string, srv *Conn) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.targets[addr] = srv
}

func (d *Driver) Dial(ctx context.Context, ep transport.Endpoint, _ transport.Identity) (transport.Conn, error) {
	d.mu.Lock()
	srv, ok := d.targets[ep.Addr]
	d.mu.Unlock()
	if !ok {
		return nil, errors.New("loopback: no listener at " + ep.Addr)
	}
	return srv.peer, nil
}

// Conn is the loopback Transport.Conn implementation.
type Conn struct {
	peerID  ids.IdentityID
	opens   chan *streamPair
	accepts chan *streamPair
	peer    *Conn
	closeMu sync.Mutex
	closed  bool
	done    chan struct{}
}

func (c *Conn) OpenStreamSync(ctx context.Context) (transport.Stream, error) {
	if c.isClosed() {
		return nil, errors.New("loopback: conn closed")
	}
	a, b := newStreamPair()
	select {
	case c.opens <- &streamPair{a: b, b: a}:
		// local side keeps a, peer's AcceptStream gets b
		return a, nil
	case <-c.done:
		return nil, io.ErrClosedPipe
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Conn) AcceptStream(ctx context.Context) (transport.Stream, error) {
	select {
	case sp := <-c.accepts:
		return sp.a, nil
	case <-c.done:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Conn) PeerIdentityID() ids.IdentityID { return c.peerID }

func (c *Conn) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	close(c.done)
	if c.peer != nil {
		// ripple the close so the peer's blocked Reads/Accepts return.
		c.peer.closeOnce()
	}
	return nil
}

func (c *Conn) closeOnce() {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	close(c.done)
}

func (c *Conn) isClosed() bool {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	return c.closed
}

func (c *Conn) Done() <-chan struct{} { return c.done }

// --- stream pair ------------------------------------------------------

type streamPair struct {
	a *stream
	b *stream
}

type stream struct {
	in   net.Conn // we use net.Pipe under the hood for io.ReadWriter semantics
	out  net.Conn
	once sync.Once
}

func newStreamPair() (*stream, *stream) {
	aIn, bOut := net.Pipe()
	bIn, aOut := net.Pipe()
	a := &stream{in: aIn, out: aOut}
	b := &stream{in: bIn, out: bOut}
	return a, b
}

func (s *stream) Read(p []byte) (int, error)  { return s.in.Read(p) }
func (s *stream) Write(p []byte) (int, error) { return s.out.Write(p) }
func (s *stream) Close() error {
	s.once.Do(func() { _ = s.in.Close(); _ = s.out.Close() })
	return nil
}
func (s *stream) CloseWrite() error { return s.out.Close() }

package server

import (
	"context"
	"net/netip"
	"sync"
	"sync/atomic"

	quicgo "github.com/quic-go/quic-go"

	"github.com/token-bay/token-bay/shared/ids"
)

// quicConn is the slice of *quicgo.Conn the server uses; declared as
// an interface so connection_test can swap a fake.
type quicConn interface {
	OpenStreamSync(ctx context.Context) (*quicgo.Stream, error)
	AcceptStream(ctx context.Context) (*quicgo.Stream, error)
	Context() context.Context
	CloseWithError(quicgo.ApplicationErrorCode, string) error
}

// Connection is the per-peer server-side state.
type Connection struct {
	peerID     ids.IdentityID
	remoteAddr netip.AddrPort
	conn       quicConn

	ctx    context.Context
	cancel context.CancelFunc

	// streamSeq tracks how many client-initiated streams we've accepted.
	// Stream #1 is the heartbeat; #2+ are RPC streams.
	streamSeq atomic.Int64

	closeOnce sync.Once
}

func newConnection(parent context.Context, qc quicConn, peerID ids.IdentityID, addr netip.AddrPort) *Connection {
	ctx, cancel := context.WithCancel(parent)
	return &Connection{
		peerID:     peerID,
		remoteAddr: addr,
		conn:       qc,
		ctx:        ctx,
		cancel:     cancel,
	}
}

// PeerID returns the mTLS-derived identity. Stable for the connection's
// lifetime.
func (c *Connection) PeerID() ids.IdentityID { return c.peerID }

// RemoteAddr returns the peer's UDP address as observed by quic-go.
func (c *Connection) RemoteAddr() netip.AddrPort { return c.remoteAddr }

// Done returns a channel that's closed when the connection ctx is
// canceled (server shutdown OR peer disconnect).
func (c *Connection) Done() <-chan struct{} { return c.ctx.Done() }

// Close cancels the connection context and tears down the underlying
// QUIC conn. Idempotent.
func (c *Connection) Close() {
	c.closeOnce.Do(func() {
		c.cancel()
		_ = c.conn.CloseWithError(0, "server close")
	})
}

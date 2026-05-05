// Package transport defines the network seam for trackerclient.
// Concrete drivers live in subpackages (loopback for tests, quic for prod).
package transport

import (
	"context"
	"crypto/ed25519"

	"github.com/token-bay/token-bay/shared/ids"
)

// Endpoint is the transport's view of a tracker. Mirrors
// trackerclient.TrackerEndpoint but keeps internal/transport free of
// import cycles.
type Endpoint struct {
	Addr         string
	IdentityHash [32]byte
}

// Identity is the local-side keypair holder.
type Identity interface {
	PrivateKey() ed25519.PrivateKey
	IdentityID() ids.IdentityID
}

// Transport dials a tracker.
type Transport interface {
	Dial(ctx context.Context, ep Endpoint, id Identity) (Conn, error)
}

// Conn is a connected, mTLS-authenticated transport session.
type Conn interface {
	OpenStreamSync(ctx context.Context) (Stream, error)
	AcceptStream(ctx context.Context) (Stream, error)
	PeerIdentityID() ids.IdentityID
	Close() error
	Done() <-chan struct{}
}

// Stream is a bidirectional byte stream within a Conn.
type Stream interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Close() error
	CloseWrite() error
}

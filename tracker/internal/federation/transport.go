package federation

import (
	"context"
	"crypto/ed25519"
)

// MaxFrameBytes is the hard cap on a single transport frame.
const MaxFrameBytes = 1 << 20 // 1 MiB

// Transport is the connection plane between trackers. The slice ships an
// in-process implementation; a real QUIC/TLS transport implements the
// same interface in a follow-up subsystem.
type Transport interface {
	Dial(ctx context.Context, addr string, expectedPeer ed25519.PublicKey) (PeerConn, error)
	Listen(ctx context.Context, accept func(PeerConn)) error
	Close() error
}

// PeerConn is one peer-to-peer link. Frames are length-delimited at the
// transport boundary (the transport itself enforces MaxFrameBytes); this
// interface deals in already-framed payloads.
type PeerConn interface {
	Send(ctx context.Context, frame []byte) error
	Recv(ctx context.Context) ([]byte, error)
	RemoteAddr() string
	RemotePub() ed25519.PublicKey
	Close() error
}

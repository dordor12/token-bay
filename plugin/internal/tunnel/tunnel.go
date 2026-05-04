package tunnel

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/quic-go/quic-go"
)

// Tunnel is one open consumer↔seeder QUIC connection wrapping one
// bidirectional stream. Methods are safe for concurrent use only in
// the documented half-duplex order: write the request fully before
// reading the response.
type Tunnel struct {
	conn   *quic.Conn
	stream *quic.Stream
	cfg    Config

	mu     sync.Mutex
	closed bool
}

// Send writes the consumer→seeder request body in length-prefixed form.
// Returns ErrTunnelClosed if Close has been called.
func (t *Tunnel) Send(body []byte) error {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return ErrTunnelClosed
	}
	return writeRequest(t.stream, body)
}

// Receive returns the seeder's response status and an io.Reader of the
// remaining content bytes (until the peer closes write). status==statusError
// implies the reader yields the UTF-8 error message.
func (t *Tunnel) Receive(ctx context.Context) (status, io.Reader, error) {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return 0, nil, ErrTunnelClosed
	}
	st, err := readResponseStatus(t.stream)
	if err != nil {
		return 0, nil, err
	}
	return st, t.stream, nil
}

// Close tears down the QUIC connection. Idempotent.
func (t *Tunnel) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()
	if t.stream != nil {
		_ = t.stream.Close()
	}
	if t.conn != nil {
		return t.conn.CloseWithError(0, "tunnel closed")
	}
	return nil
}

// errIs is shorthand used by Dial / Accept implementations.
//
//nolint:unused // reserved for symmetric Accept flow (Task 8)
func errIs(err error, target error) bool { return errors.Is(err, target) }

package tunnel

import (
	"context"
	"io"
	"sync"
	"time"

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
// If Close has *completed* before Send is entered, returns ErrTunnelClosed.
// Concurrent Close after the closed-check yields a quic-wrapped network
// error from the underlying stream.Write — Tunnel is half-duplex single-
// stream by contract; callers should not race Close against Send.
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
// remaining content bytes (until the peer closes write or the QUIC
// connection terminates). status==statusError implies the reader yields
// the UTF-8 error message.
//
// If ctx has a deadline, it is propagated to the stream's read path
// (SetReadDeadline). ctx cancellation interrupts any in-flight read by
// setting the read deadline to time.Now(); the caller will receive a
// QUIC-wrapped error.
//
// If Close has *completed* before Receive is entered, returns
// ErrTunnelClosed. Concurrent Close after the closed-check yields a
// quic-wrapped network error from the underlying stream.Read — Tunnel is
// half-duplex single-stream by contract; callers should not race Close
// against Receive.
func (t *Tunnel) Receive(ctx context.Context) (status, io.Reader, error) {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return 0, nil, ErrTunnelClosed
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = t.stream.SetReadDeadline(dl)
	}
	doneCh := make(chan struct{})
	defer close(doneCh)
	go func() {
		select {
		case <-ctx.Done():
			_ = t.stream.SetReadDeadline(time.Now())
		case <-doneCh:
		}
	}()
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

package trackerclient

import (
	"context"
	"errors"
	"sync"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
)

// connHolder gives RPC callers a way to await the current Conn.
type connHolder struct {
	mu      sync.RWMutex
	current transport.Conn
	waiters []chan transport.Conn
	closed  bool
}

func (h *connHolder) get(ctx context.Context) (transport.Conn, error) {
	h.mu.RLock()
	if h.closed {
		h.mu.RUnlock()
		return nil, ErrClosed
	}
	if h.current != nil {
		c := h.current
		h.mu.RUnlock()
		return c, nil
	}
	h.mu.RUnlock()

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, ErrClosed
	}
	if h.current != nil {
		c := h.current
		h.mu.Unlock()
		return c, nil
	}
	ch := make(chan transport.Conn, 1)
	h.waiters = append(h.waiters, ch)
	h.mu.Unlock()

	select {
	case c := <-ch:
		if c == nil {
			return nil, ErrClosed
		}
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (h *connHolder) set(c transport.Conn) {
	h.mu.Lock()
	h.current = c
	waiters := h.waiters
	h.waiters = nil
	h.mu.Unlock()
	for _, w := range waiters {
		select {
		case w <- c:
		default:
		}
	}
}

func (h *connHolder) clear() {
	h.mu.Lock()
	h.current = nil
	h.mu.Unlock()
}

func (h *connHolder) close() {
	h.mu.Lock()
	h.closed = true
	waiters := h.waiters
	h.waiters = nil
	h.mu.Unlock()
	for _, w := range waiters {
		close(w)
	}
}

//nolint:unused // surfaced via supervisor in future tasks
var errSupervisorStopped = errors.New("trackerclient: supervisor stopped")

package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/quic-go/quic-go"
)

// Dial opens a QUIC connection to the seeder at addr and a single
// bidirectional stream. Returns *Tunnel on success.
//
// The handshake is bounded by ctx; pass context.WithTimeout for the
// 2-second budget the consumer fallback path needs (see plugin design
// spec §5.4 — propagation + handshake must fit inside the user-visible
// "next message" latency).
func Dial(ctx context.Context, addr netip.AddrPort, cfg Config) (*Tunnel, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	tlsCfg, err := newTLSConfig(cfg.EphemeralPriv, cfg.PeerPin, cfg.Now(), false)
	if err != nil {
		return nil, err
	}
	quicCfg := &quic.Config{
		HandshakeIdleTimeout: cfg.HandshakeTimeout,
		MaxIdleTimeout:       cfg.IdleTimeout,
	}

	hsCtx, cancel := context.WithTimeout(ctx, cfg.HandshakeTimeout)
	defer cancel()

	udp := &net.UDPAddr{IP: addr.Addr().AsSlice(), Port: int(addr.Port())}
	conn, err := quic.DialAddr(hsCtx, udp.String(), tlsCfg, quicCfg)
	if err != nil {
		return nil, mapHandshakeErr(err)
	}

	stream, err := conn.OpenStreamSync(hsCtx)
	if err != nil {
		_ = conn.CloseWithError(0, "stream open failed")
		return nil, fmt.Errorf("%w: open stream: %v", ErrHandshakeFailed, err)
	}

	return &Tunnel{conn: conn, stream: stream, cfg: cfg}, nil
}

// mapHandshakeErr inspects err and surfaces ErrPeerPinMismatch / ErrALPNMismatch
// when their underlying signal is recognizable; otherwise wraps ErrHandshakeFailed.
//
// The TLS handshake error message from quic-go embeds the alert string. We pattern-match
// to surface the structured sentinel; the underlying error is preserved via wrapping.
func mapHandshakeErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	msg := err.Error()
	switch {
	case errors.Is(err, ErrPeerPinMismatch):
		return err
	case strings.Contains(msg, "tunnel: peer pin mismatch"):
		return fmt.Errorf("%w: %v", ErrPeerPinMismatch, err)
	case strings.Contains(msg, "no application protocol"),
		strings.Contains(msg, "ALPN"):
		return fmt.Errorf("%w: %v", ErrALPNMismatch, err)
	default:
		return fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
}

// timeoutBudget returns the soonest deadline between ctx and now+d.
// Helper exported for symmetry with Accept; unused here but kept for the listener.
//
//nolint:unused // reserved for Task 8 listen.go
func timeoutBudget(ctx context.Context, d time.Duration) time.Time {
	dl, ok := ctx.Deadline()
	if !ok {
		return time.Now().Add(d)
	}
	other := time.Now().Add(d)
	if dl.Before(other) {
		return dl
	}
	return other
}

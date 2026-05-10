package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/quic-go/quic-go"
)

// Dial opens a QUIC connection to the seeder at addr and a single
// bidirectional stream. Returns *Tunnel on success.
//
// The handshake is bounded by ctx; pass context.WithTimeout for the
// 2-second budget the consumer fallback path needs (see plugin design
// spec §5.4 — propagation + handshake must fit inside the user-visible
// "next message" latency).
//
// When cfg.Rendezvous is non-nil, Dial switches to rendezvous mode (see
// dialRendezvous): a private quic.Transport is bound, AllocateReflexive
// is called, a short UDP punch burst is sent toward addr, and the QUIC
// handshake is bounded by cfg.HolePunchTimeout. On hole-punch failure
// the path falls back to OpenRelay.
func Dial(ctx context.Context, addr netip.AddrPort, cfg Config) (*Tunnel, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Rendezvous != nil {
		return dialRendezvous(ctx, addr, cfg)
	}
	return dialDirect(ctx, addr, cfg)
}

func dialDirect(ctx context.Context, addr netip.AddrPort, cfg Config) (*Tunnel, error) {
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

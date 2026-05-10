package ccproxy

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/token-bay/token-bay/plugin/internal/tunnel"
)

// tunnelDialer is the production PeerDialer wrapping internal/tunnel.Dial.
// Field-only construction keeps tests honest: a zero-value tunnelDialer
// applies sensible defaults via resolveTimeouts.
type tunnelDialer struct {
	DialTimeout time.Duration // default 5s
	Now         func() time.Time
}

// NewTunnelDialer returns a PeerDialer that opens a real tunnel.Tunnel
// per call. Defaults: 5s dial timeout, time.Now clock.
func NewTunnelDialer() PeerDialer {
	return &tunnelDialer{}
}

func (d *tunnelDialer) resolveTimeout() time.Duration {
	if d.DialTimeout > 0 {
		return d.DialTimeout
	}
	return 5 * time.Second
}

func (d *tunnelDialer) resolveNow() func() time.Time {
	if d.Now != nil {
		return d.Now
	}
	return time.Now
}

// Dial validates meta, then opens a tunnel.Tunnel.
func (d *tunnelDialer) Dial(ctx context.Context, meta *EntryMetadata) (PeerConn, error) {
	if meta == nil {
		return nil, ErrNoAssignment
	}
	if !meta.SeederAddr.IsValid() {
		return nil, fmt.Errorf("%w: invalid SeederAddr", ErrNoAssignment)
	}
	if len(meta.SeederPubkey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: SeederPubkey wrong size", ErrNoAssignment)
	}
	if len(meta.EphemeralPriv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: EphemeralPriv wrong size", ErrNoAssignment)
	}

	dialCtx, cancel := context.WithTimeout(ctx, d.resolveTimeout())
	defer cancel()

	cfg := tunnel.Config{
		EphemeralPriv: meta.EphemeralPriv,
		PeerPin:       meta.SeederPubkey,
		Now:           d.resolveNow(),
	}
	tun, err := tunnel.Dial(dialCtx, meta.SeederAddr, cfg)
	if err != nil {
		// Map sentinels to short, stable strings for the router's
		// sanitizeDialErr to consume. errors.Is preserves chains.
		switch {
		case errors.Is(err, tunnel.ErrPeerPinMismatch):
			return nil, fmt.Errorf("peer pin mismatch: %w", err)
		case errors.Is(err, tunnel.ErrALPNMismatch):
			return nil, fmt.Errorf("alpn mismatch: %w", err)
		case errors.Is(err, tunnel.ErrHandshakeFailed):
			return nil, fmt.Errorf("handshake failed: %w", err)
		case errors.Is(err, tunnel.ErrInvalidConfig):
			return nil, fmt.Errorf("invalid tunnel config: %w", err)
		default:
			return nil, fmt.Errorf("tunnel dial: %w", err)
		}
	}
	return &tunnelPeerConn{tun: tun}, nil
}

// tunnelPeerConn adapts a *tunnel.Tunnel to PeerConn.
type tunnelPeerConn struct {
	tun *tunnel.Tunnel
}

func (p *tunnelPeerConn) Send(body []byte) error {
	return p.tun.Send(body)
}

func (p *tunnelPeerConn) Receive(ctx context.Context) (bool, string, io.Reader, error) {
	st, r, err := p.tun.Receive(ctx)
	if err != nil {
		return false, "", nil, err
	}
	switch st {
	case tunnel.StatusOK:
		return true, "", r, nil
	case tunnel.StatusError:
		// readAll up to 4 KiB — the wire-format bound on error messages.
		const errCap = 4 << 10
		buf, _ := io.ReadAll(io.LimitReader(r, errCap))
		return false, string(buf), nil, nil
	default:
		return false, "", nil, fmt.Errorf("unknown tunnel status: 0x%02x", byte(st))
	}
}

func (p *tunnelPeerConn) Close() error { return p.tun.Close() }

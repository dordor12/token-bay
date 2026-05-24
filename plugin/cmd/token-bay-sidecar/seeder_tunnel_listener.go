package main

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net/netip"

	"github.com/token-bay/token-bay/plugin/internal/seederflow"
	"github.com/token-bay/token-bay/plugin/internal/tunnel"
)

// defaultSeederTunnelBindAddr is the UDP bind address the seeder uses
// for inbound consumer tunnels. ":0" picks an ephemeral port; the
// concrete address is published to peers via the rendezvous flow
// (separate plan), not via the cmd layer's static config.
const defaultSeederTunnelBindAddr = "0.0.0.0:0"

// newSeederTunnelListenerFactory returns a seederflow.TunnelListenerFactory
// that wraps internal/tunnel.Listen. Each Bind allocates a fresh
// UDP socket so the listener's TLS pin (seederPriv + consumerPub)
// matches exactly one consumer's per-session ephemeral keypair —
// QUIC + TLS pinning then enforces that any other dialer's handshake
// fails closed.
func newSeederTunnelListenerFactory() seederflow.TunnelListenerFactory {
	bind, err := netip.ParseAddrPort(defaultSeederTunnelBindAddr)
	if err != nil {
		// defaultSeederTunnelBindAddr is a hard-coded literal; a parse
		// failure here would be a programmer error caught by the binary's
		// unit tests, so panicking is appropriate.
		panic(fmt.Sprintf("invalid defaultSeederTunnelBindAddr %q: %v", defaultSeederTunnelBindAddr, err))
	}
	return func(seederPriv ed25519.PrivateKey, consumerPub ed25519.PublicKey) (seederflow.TunnelListener, error) {
		ln, err := tunnel.Listen(bind, tunnel.Config{
			EphemeralPriv: seederPriv,
			PeerPin:       consumerPub,
		})
		if err != nil {
			return nil, fmt.Errorf("tunnel listen: %w", err)
		}
		return &tunnelListenerAdapter{ln: ln}, nil
	}
}

// tunnelListenerAdapter wraps *tunnel.Listener so the *tunnel.Tunnel
// returned from Accept satisfies seederflow.TunnelConn directly (the
// methods line up: ReadRequest, SendOK, ResponseWriter, SendError,
// CloseWrite, Close).
type tunnelListenerAdapter struct {
	ln *tunnel.Listener
}

func (a *tunnelListenerAdapter) Accept(ctx context.Context) (seederflow.TunnelConn, error) {
	tun, err := a.ln.Accept(ctx)
	if err != nil {
		return nil, err
	}
	return tun, nil
}

func (a *tunnelListenerAdapter) LocalAddr() netip.AddrPort { return a.ln.LocalAddr() }

func (a *tunnelListenerAdapter) Close() error { return a.ln.Close() }

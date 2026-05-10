package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/quic-go/quic-go"
)

// holePunchProbeBytes is the harmless payload of a UDP punch packet. The
// content is irrelevant; what matters is that the packets traverse the
// caller's NAT outbound, opening a 5-tuple state entry that lets the peer's
// QUIC INITIAL packet arrive.
var holePunchProbeBytes = []byte{0}

// holePunchProbeCount is how many redundant punch packets to send. Three
// packets cover typical loss-tolerance for a single NAT state-establishment
// exchange; tests on 127.0.0.1 do not need any but the loop is cheap.
const holePunchProbeCount = 3

// holePunchProbeInterval paces the probes. Loose enough that a slow NAT has
// time to establish state between bursts; tight enough that 3 probes cost
// <100ms total.
const holePunchProbeInterval = 25 * time.Millisecond

// dialRendezvous is the rendezvous-mode entry point invoked by Dial when
// cfg.Rendezvous is non-nil. It binds an ephemeral UDP socket, asks the
// rendezvous oracle for the local reflexive address (currently used for
// telemetry / future broker advertisement; failure is fatal because the
// caller depends on the peer learning this address out-of-band), sends a
// short burst of UDP punch packets, then attempts a QUIC handshake against
// peerAddr. On timeout it falls back to the relay path (Task 4).
func dialRendezvous(ctx context.Context, peerAddr netip.AddrPort, cfg Config) (*Tunnel, error) {
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("%w: bind udp: %v", ErrInvalidConfig, err)
	}

	if _, err := cfg.Rendezvous.AllocateReflexive(ctx); err != nil {
		_ = udp.Close()
		return nil, fmt.Errorf("rendezvous: allocate reflexive: %w", err)
	}

	transport := &quic.Transport{Conn: udp}

	// Punch: a short burst to open the local NAT mapping toward peerAddr.
	// Failures are swallowed — the actual handshake below is the success
	// signal.
	punchTo := net.UDPAddrFromAddrPort(peerAddr)
	for range holePunchProbeCount {
		_, _ = transport.WriteTo(holePunchProbeBytes, punchTo)
		select {
		case <-time.After(holePunchProbeInterval):
		case <-ctx.Done():
			_ = transport.Close()
			return nil, fmt.Errorf("%w: %v", ErrHolePunchFailed, ctx.Err())
		}
	}

	tun, err := dialOverTransport(ctx, transport, peerAddr, cfg, cfg.HolePunchTimeout)
	if err == nil {
		return tun, nil
	}
	hpErr := err

	// Hole-punch failed — fall back to TURN-style relay.
	relay, err := cfg.Rendezvous.OpenRelay(ctx, cfg.SessionID)
	if err != nil {
		_ = transport.Close()
		return nil, fmt.Errorf("%w: %w", ErrRelayFailed, err)
	}
	if !relay.Endpoint.IsValid() {
		_ = transport.Close()
		return nil, fmt.Errorf("%w: relay endpoint invalid (zero AddrPort)", ErrRelayFailed)
	}

	// Punch toward the relay endpoint to open the local NAT mapping toward
	// the tracker (in production paths the relay is the tracker).
	relayPunchTo := net.UDPAddrFromAddrPort(relay.Endpoint)
	for range holePunchProbeCount {
		_, _ = transport.WriteTo(holePunchProbeBytes, relayPunchTo)
		select {
		case <-time.After(holePunchProbeInterval):
		case <-ctx.Done():
			_ = transport.Close()
			return nil, fmt.Errorf("%w: %v", ErrRelayFailed, ctx.Err())
		}
	}

	tun, err = dialOverTransport(ctx, transport, relay.Endpoint, cfg, cfg.HandshakeTimeout)
	if err != nil {
		_ = transport.Close()
		// Surface ErrPeerPinMismatch / ErrALPNMismatch verbatim — those
		// are security signals and must not be hidden by the relay-fallback
		// wrapper. Other errors get joined with the original hole-punch
		// failure for diagnostic surface.
		if errors.Is(err, ErrPeerPinMismatch) || errors.Is(err, ErrALPNMismatch) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: hole-punch: %v; relay: %v", ErrRelayFailed, hpErr, err)
	}
	return tun, nil
}

// dialOverTransport runs the QUIC + TLS handshake against addr using the
// provided transport, then opens the bidirectional stream. Returns a
// *Tunnel that owns transport; caller must not close transport on success.
func dialOverTransport(ctx context.Context, transport *quic.Transport, addr netip.AddrPort, cfg Config, timeout time.Duration) (*Tunnel, error) {
	tlsCfg, err := newTLSConfig(cfg.EphemeralPriv, cfg.PeerPin, cfg.Now(), false)
	if err != nil {
		return nil, err
	}
	quicCfg := &quic.Config{
		HandshakeIdleTimeout: timeout,
		MaxIdleTimeout:       cfg.IdleTimeout,
	}
	hsCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := transport.Dial(hsCtx, net.UDPAddrFromAddrPort(addr), tlsCfg, quicCfg)
	if err != nil {
		return nil, mapHandshakeErr(err)
	}
	stream, err := conn.OpenStreamSync(hsCtx)
	if err != nil {
		_ = conn.CloseWithError(0, "stream open failed")
		return nil, fmt.Errorf("%w: open stream: %v", ErrHandshakeFailed, err)
	}
	return &Tunnel{conn: conn, stream: stream, cfg: cfg, ownsTransport: transport}, nil
}

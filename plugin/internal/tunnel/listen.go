package tunnel

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// defaultStunAllocateTimeout caps the rendezvous AllocateReflexive call
// invoked by Listen on bind. The tracker round-trip is small (sub-100ms
// in steady state); 5s gives generous headroom for retry-laden reconnect
// paths inside trackerclient without blocking Listen indefinitely.
const defaultStunAllocateTimeout = 5 * time.Second

// Listener is the seeder-side accept loop. One Listener may produce
// many *Tunnel via Accept. Accept is safe to call from multiple
// goroutines; Close is idempotent and may race with in-flight Accept
// calls (those return a quic-wrapped error after Close lands).
type Listener struct {
	cfg       Config
	transport *quic.Transport
	udp       *net.UDPConn
	ln        *quic.Listener
	addr      netip.AddrPort
	reflexive netip.AddrPort

	mu     sync.Mutex
	closed bool
}

// Listen binds a UDP socket at bind and starts accepting QUIC connections.
// bind.Port == 0 allocates an ephemeral port; LocalAddr returns the
// resolved AddrPort after Listen returns.
func Listen(bind netip.AddrPort, cfg Config) (*Listener, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if !bind.Addr().IsValid() {
		return nil, fmt.Errorf("%w: bind address invalid", ErrInvalidConfig)
	}

	udpAddr := &net.UDPAddr{IP: bind.Addr().AsSlice(), Port: int(bind.Port())}
	udp, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("%w: listen udp %s: %v", ErrInvalidConfig, bind, err)
	}

	tlsCfg, err := newTLSConfig(cfg.EphemeralPriv, cfg.PeerPin, cfg.Now(), true)
	if err != nil {
		_ = udp.Close()
		return nil, err
	}
	quicCfg := &quic.Config{
		HandshakeIdleTimeout: cfg.HandshakeTimeout,
		MaxIdleTimeout:       cfg.IdleTimeout,
	}
	tr := &quic.Transport{Conn: udp}
	ln, err := tr.Listen(tlsCfg, quicCfg)
	if err != nil {
		_ = tr.Close()
		_ = udp.Close()
		return nil, fmt.Errorf("%w: quic listen: %v", ErrInvalidConfig, err)
	}

	resolved := udp.LocalAddr().(*net.UDPAddr).AddrPort()

	var reflexive netip.AddrPort
	if cfg.Rendezvous != nil {
		ctx, cancel := context.WithTimeout(context.Background(), defaultStunAllocateTimeout)
		defer cancel()
		addr, err := cfg.Rendezvous.AllocateReflexive(ctx)
		if err != nil {
			_ = ln.Close()
			_ = tr.Close()
			_ = udp.Close()
			return nil, fmt.Errorf("rendezvous: allocate reflexive: %w", err)
		}
		reflexive = addr
	}

	return &Listener{
		cfg:       cfg,
		transport: tr,
		udp:       udp,
		ln:        ln,
		addr:      resolved,
		reflexive: reflexive,
	}, nil
}

// ReflexiveAddr returns the address the configured Rendezvous reported when
// Listen called AllocateReflexive. Zero AddrPort if Listen was invoked
// without a Rendezvous; the caller (orchestration layer) advertises this
// address through the broker so consumers can dial it during hole-punch.
func (l *Listener) ReflexiveAddr() netip.AddrPort { return l.reflexive }

// LocalAddr returns the bound address.
func (l *Listener) LocalAddr() netip.AddrPort { return l.addr }

// Accept returns the next handshaked tunnel. Blocks until a peer
// completes the QUIC + TLS handshake AND opens the bidi stream, or
// ctx fires. After Close, in-flight or subsequent Accept calls return
// a quic-wrapped error (mapped via mapHandshakeErr).
func (l *Listener) Accept(ctx context.Context) (*Tunnel, error) {
	conn, err := l.ln.Accept(ctx)
	if err != nil {
		return nil, mapHandshakeErr(err)
	}
	stream, err := conn.AcceptStream(ctx)
	if err != nil {
		_ = conn.CloseWithError(0, "stream accept failed")
		return nil, fmt.Errorf("%w: accept stream: %v", ErrHandshakeFailed, err)
	}
	return &Tunnel{conn: conn, stream: stream, cfg: l.cfg}, nil
}

// Close tears down the listener. Idempotent.
func (l *Listener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()
	_ = l.ln.Close()
	_ = l.transport.Close()
	return l.udp.Close()
}

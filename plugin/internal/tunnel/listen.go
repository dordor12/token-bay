package tunnel

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"

	"github.com/quic-go/quic-go"
)

// Listener is the seeder-side accept loop. One Listener may produce
// many *Tunnel via Accept.
type Listener struct {
	cfg       Config
	transport *quic.Transport
	udp       *net.UDPConn
	ln        *quic.Listener
	addr      netip.AddrPort

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

	return &Listener{
		cfg:       cfg,
		transport: tr,
		udp:       udp,
		ln:        ln,
		addr:      resolved,
	}, nil
}

// LocalAddr returns the bound address.
func (l *Listener) LocalAddr() netip.AddrPort { return l.addr }

// Accept returns the next handshaked tunnel. Blocks until a peer
// completes the QUIC + TLS handshake AND opens the bidi stream, or
// ctx fires.
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

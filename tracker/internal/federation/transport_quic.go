package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	quicgo "github.com/quic-go/quic-go"
)

// QUICConfig is the runtime configuration for QUICTransport.
type QUICConfig struct {
	ListenAddr  string          // host:port; empty disables Listen but Dial still works.
	IdleTimeout time.Duration   // QUIC max-idle; default 60s if zero.
	Cert        tls.Certificate // built via CertFromIdentity(myPriv).
	HandshakeTO time.Duration   // dial-side handshake timeout; default 5s if zero.
}

func (c QUICConfig) withDefaults() QUICConfig {
	if c.IdleTimeout == 0 {
		c.IdleTimeout = 60 * time.Second
	}
	if c.HandshakeTO == 0 {
		c.HandshakeTO = 5 * time.Second
	}
	return c
}

// QUICTransport implements Transport over quic-go + mTLS. Construct via
// NewQUICTransport.
type QUICTransport struct {
	cfg QUICConfig

	mu         sync.Mutex
	closed     bool
	listener   *quicgo.Listener
	listenAddr string
}

// NewQUICTransport binds the listener (if ListenAddr != "") and returns a
// ready transport. If ListenAddr is empty, Listen returns immediately and
// the transport is dial-only.
func NewQUICTransport(cfg QUICConfig) (*QUICTransport, error) {
	cfg = cfg.withDefaults()
	if len(cfg.Cert.Certificate) == 0 {
		return nil, errors.New("federation: QUICConfig.Cert is empty")
	}
	t := &QUICTransport{cfg: cfg}
	if cfg.ListenAddr == "" {
		return t, nil
	}

	udpAddr, err := net.ResolveUDPAddr("udp", cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("federation: resolve %q: %w", cfg.ListenAddr, err)
	}
	pc, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("federation: listen UDP %q: %w", cfg.ListenAddr, err)
	}

	tlsCfg := MakeServerTLSConfig(cfg.Cert, nil) // SPKI captured per-conn at Accept time.
	qcfg := &quicgo.Config{
		EnableDatagrams: false,
		Allow0RTT:       false,
		MaxIdleTimeout:  cfg.IdleTimeout,
	}
	l, err := quicgo.Listen(pc, tlsCfg, qcfg)
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("federation: quic.Listen: %w", err)
	}
	t.listener = l
	t.listenAddr = pc.LocalAddr().String()
	return t, nil
}

// ListenAddr returns the bound address (resolved port for ":0" binds).
// Empty if the transport has no listener.
func (t *QUICTransport) ListenAddr() string { return t.listenAddr }

// Dial implements Transport.Dial.
func (t *QUICTransport) Dial(ctx context.Context, addr string, expectedPeer ed25519.PublicKey) (PeerConn, error) {
	if len(expectedPeer) != ed25519.PublicKeySize {
		return nil, errors.New("federation: expectedPeer must be 32 bytes")
	}
	expectedSPKI, err := spkiOfPubkey(expectedPeer)
	if err != nil {
		return nil, fmt.Errorf("federation: spki of expectedPeer: %w", err)
	}
	tlsCfg := MakeClientTLSConfig(t.cfg.Cert, expectedSPKI)
	qcfg := &quicgo.Config{
		EnableDatagrams: false,
		Allow0RTT:       false,
		MaxIdleTimeout:  t.cfg.IdleTimeout,
	}
	dialCtx, cancel := context.WithTimeout(ctx, t.cfg.HandshakeTO)
	defer cancel()
	conn, err := quicgo.DialAddr(dialCtx, addr, tlsCfg, qcfg)
	if err != nil {
		return nil, fmt.Errorf("federation: quic.DialAddr %q: %w", addr, err)
	}
	return newQConn(conn, conn.OpenStreamSync, t.cfg.HandshakeTO, expectedPeer, addr), nil
}

// Listen implements Transport.Listen. Blocks until ctx is canceled.
func (t *QUICTransport) Listen(ctx context.Context, accept func(PeerConn)) error {
	if t.listener == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		conn, err := t.listener.Accept(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			continue
		}
		go t.handleAccept(ctx, conn, accept)
	}
}

func (t *QUICTransport) handleAccept(_ context.Context, conn *quicgo.Conn, accept func(PeerConn)) {
	state := conn.ConnectionState().TLS
	if len(state.PeerCertificates) != 1 {
		_ = conn.CloseWithError(0, "bad peer certs")
		return
	}
	peerCert := state.PeerCertificates[0]
	peerPub, ok := peerCert.PublicKey.(ed25519.PublicKey)
	if !ok {
		_ = conn.CloseWithError(0, "non-ed25519")
		return
	}
	// Lazy-accept the stream on first Recv. QUIC streams are only
	// visible to the remote after data is sent, so blocking on
	// AcceptStream here would deadlock callers that wait on accept
	// before the dialer Sends.
	accept(newQConn(conn, conn.AcceptStream, t.cfg.HandshakeTO, peerPub, conn.RemoteAddr().String()))
}

// Close closes the listener and rejects future Dials. Idempotent.
func (t *QUICTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	if t.listener != nil {
		return t.listener.Close()
	}
	return nil
}

// spkiOfPubkey returns sha256(SubjectPublicKeyInfo) for a synthetic cert
// wrapping pub. Used by Dial to compute the pin without minting a full
// cert at the call site.
func spkiOfPubkey(pub ed25519.PublicKey) ([32]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(der), nil
}

// qConn is one peer-to-peer link backed by a *quicgo.Conn + a single
// persistent bidi stream. The stream is opened (or accepted) lazily on
// the first Send/Recv — QUIC requires data flow before a stream becomes
// visible to the remote, so blocking on AcceptStream synchronously would
// deadlock the listener-side handshake before the dialer sends Hello.
//
// Frames are length-delimited (4-byte big-endian length prefix); both
// Send and Recv enforce MaxFrameBytes.
type qConn struct {
	conn        *quicgo.Conn
	streamFn    func(context.Context) (*quicgo.Stream, error)
	handshakeTO time.Duration

	streamOnce sync.Once
	stream     *quicgo.Stream
	streamErr  error

	remotePub  ed25519.PublicKey
	remoteAddr string

	closeOnce sync.Once

	sendMu sync.Mutex
	recvMu sync.Mutex
}

func newQConn(
	conn *quicgo.Conn,
	streamFn func(context.Context) (*quicgo.Stream, error),
	handshakeTO time.Duration,
	peerPub ed25519.PublicKey,
	addr string,
) *qConn {
	return &qConn{
		conn:        conn,
		streamFn:    streamFn,
		handshakeTO: handshakeTO,
		remotePub:   peerPub,
		remoteAddr:  addr,
	}
}

// ensureStream materializes the persistent bidi stream on first call.
// streamFn is conn.OpenStreamSync (dialer side) or conn.AcceptStream
// (listener side). Both block until the underlying QUIC machinery is
// ready; we wrap with handshakeTO to bound the wait.
func (q *qConn) ensureStream(ctx context.Context) error {
	q.streamOnce.Do(func() {
		sCtx, cancel := context.WithTimeout(ctx, q.handshakeTO)
		defer cancel()
		q.stream, q.streamErr = q.streamFn(sCtx)
	})
	return q.streamErr
}

func (q *qConn) Send(ctx context.Context, frame []byte) error {
	if len(frame) > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	if err := q.ensureStream(ctx); err != nil {
		return mapStreamErr(err)
	}
	q.sendMu.Lock()
	defer q.sendMu.Unlock()

	if dl, ok := ctx.Deadline(); ok {
		_ = q.stream.SetWriteDeadline(dl)
		defer func() { _ = q.stream.SetWriteDeadline(time.Time{}) }()
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(frame))) //nolint:gosec // len <= MaxFrameBytes (1 MiB) — checked above
	if _, err := q.stream.Write(hdr[:]); err != nil {
		return mapStreamErr(err)
	}
	if _, err := q.stream.Write(frame); err != nil {
		return mapStreamErr(err)
	}
	return nil
}

func (q *qConn) Recv(ctx context.Context) ([]byte, error) {
	if err := q.ensureStream(ctx); err != nil {
		return nil, mapStreamErr(err)
	}
	q.recvMu.Lock()
	defer q.recvMu.Unlock()

	if dl, ok := ctx.Deadline(); ok {
		_ = q.stream.SetReadDeadline(dl)
		defer func() { _ = q.stream.SetReadDeadline(time.Time{}) }()
	}
	var hdr [4]byte
	if _, err := io.ReadFull(q.stream, hdr[:]); err != nil {
		return nil, mapStreamErr(err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrameBytes {
		return nil, ErrFrameTooLarge
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(q.stream, buf); err != nil {
		return nil, mapStreamErr(err)
	}
	return buf, nil
}

func (q *qConn) RemoteAddr() string           { return q.remoteAddr }
func (q *qConn) RemotePub() ed25519.PublicKey { return q.remotePub }

func (q *qConn) Close() error {
	q.closeOnce.Do(func() {
		if q.stream != nil {
			_ = q.stream.Close()
		}
		_ = q.conn.CloseWithError(0, "qConn close")
	})
	return nil
}

// mapStreamErr converts a quic-go stream error to our sentinel error
// vocabulary where possible. Any unknown error is wrapped as ErrPeerClosed
// so callers (peer.recvLoop) can react uniformly.
func mapStreamErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) {
		return ErrPeerClosed
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrPeerClosed, err)
}

var (
	_ Transport = (*QUICTransport)(nil)
	_ PeerConn  = (*qConn)(nil)
)

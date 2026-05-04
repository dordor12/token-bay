// Package quic is the production Transport driver. Wraps quic-go and
// applies our mTLS contract from internal/idtls.
package quic

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	quicgo "github.com/quic-go/quic-go"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/idtls"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
	"github.com/token-bay/token-bay/shared/ids"
)

// Driver is a stateless transport.Transport. Each Dial brings up a fresh
// QUIC connection.
type Driver struct{}

func New() *Driver { return &Driver{} }

func (d *Driver) Dial(ctx context.Context, ep transport.Endpoint, id transport.Identity) (transport.Conn, error) {
	if id == nil {
		return nil, errors.New("quic: identity is required")
	}
	cert, err := idtls.CertFromIdentity(id.PrivateKey())
	if err != nil {
		return nil, fmt.Errorf("quic: cert: %w", err)
	}
	tlsCfg := idtls.MakeClientTLSConfig(cert, ep.IdentityHash)

	qcfg := &quicgo.Config{
		EnableDatagrams: false,
		Allow0RTT:       false,
	}
	raw, err := quicgo.DialAddr(ctx, ep.Addr, tlsCfg, qcfg)
	if err != nil {
		return nil, fmt.Errorf("quic: dial: %w", err)
	}
	state := raw.ConnectionState().TLS
	if len(state.PeerCertificates) != 1 {
		_ = raw.CloseWithError(0, "no peer cert")
		return nil, errors.New("quic: peer presented no cert")
	}
	h := sha256.Sum256(state.PeerCertificates[0].RawSubjectPublicKeyInfo)
	var pid ids.IdentityID
	copy(pid[:], h[:])

	done := make(chan struct{})
	go func() {
		<-raw.Context().Done()
		close(done)
	}()

	return &qConn{raw: raw, peerID: pid, done: done}, nil
}

type qConn struct {
	raw    *quicgo.Conn
	peerID ids.IdentityID
	done   chan struct{}
}

func (c *qConn) OpenStreamSync(ctx context.Context) (transport.Stream, error) {
	s, err := c.raw.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	return &qStream{raw: s}, nil
}

func (c *qConn) AcceptStream(ctx context.Context) (transport.Stream, error) {
	s, err := c.raw.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	return &qStream{raw: s}, nil
}

func (c *qConn) PeerIdentityID() ids.IdentityID { return c.peerID }
func (c *qConn) Done() <-chan struct{}          { return c.done }

func (c *qConn) Close() error {
	return c.raw.CloseWithError(0, "client close")
}

type qStream struct {
	raw *quicgo.Stream
}

func (s *qStream) Read(p []byte) (int, error)  { return s.raw.Read(p) }
func (s *qStream) Write(p []byte) (int, error) { return s.raw.Write(p) }
func (s *qStream) Close() error                { return s.raw.Close() }
func (s *qStream) CloseWrite() error {
	// quic-go's Stream.Close() half-closes the write side; the read side
	// continues until the peer half-closes too. The behaviour we want is
	// identical, so we delegate.
	return s.raw.Close()
}

package seederflow_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/seederflow"
)

// fakeTunnelListener is the test stand-in for *tunnel.Listener inside
// RebindingAcceptor. It records its bound peer pubkey and exposes a
// channel so tests can deliver fake tunnels or simulate a closed listener.
type fakeTunnelListener struct {
	seederPriv  ed25519.PrivateKey
	consumerPub ed25519.PublicKey
	addr        netip.AddrPort
	conns       chan seederflow.TunnelConn
	closed      atomic.Bool
}

func newFakeListener(seederPriv ed25519.PrivateKey, consumerPub ed25519.PublicKey) *fakeTunnelListener {
	return &fakeTunnelListener{
		seederPriv:  append(ed25519.PrivateKey(nil), seederPriv...),
		consumerPub: append(ed25519.PublicKey(nil), consumerPub...),
		addr:        netip.MustParseAddrPort("127.0.0.1:42"),
		conns:       make(chan seederflow.TunnelConn, 1),
	}
}

func (l *fakeTunnelListener) Accept(ctx context.Context) (seederflow.TunnelConn, error) {
	select {
	case c, ok := <-l.conns:
		if !ok {
			return nil, errors.New("fakeListener: closed")
		}
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *fakeTunnelListener) LocalAddr() netip.AddrPort { return l.addr }

func (l *fakeTunnelListener) Close() error {
	if l.closed.CompareAndSwap(false, true) {
		close(l.conns)
	}
	return nil
}

// trackingFactory records every (priv, pub) it is called with and
// returns a fresh fakeTunnelListener for each call.
type trackingFactory struct {
	mu        sync.Mutex
	created   []*fakeTunnelListener
	bindings  []stubBinding
	createErr error
}

func (f *trackingFactory) build() seederflow.TunnelListenerFactory {
	return func(seederPriv ed25519.PrivateKey, consumerPub ed25519.PublicKey) (seederflow.TunnelListener, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.createErr != nil {
			return nil, f.createErr
		}
		f.bindings = append(f.bindings, stubBinding{
			SeederPriv:  append(ed25519.PrivateKey(nil), seederPriv...),
			ConsumerPub: append(ed25519.PublicKey(nil), consumerPub...),
		})
		l := newFakeListener(seederPriv, consumerPub)
		f.created = append(f.created, l)
		return l, nil
	}
}

func (f *trackingFactory) listeners() []*fakeTunnelListener {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*fakeTunnelListener, len(f.created))
	copy(out, f.created)
	return out
}

func (f *trackingFactory) recorded() []stubBinding {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]stubBinding, len(f.bindings))
	copy(out, f.bindings)
	return out
}

func genPair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return pub, priv
}

func TestRebindingAcceptor_BindCreatesListenerWithKeys(t *testing.T) {
	factory := &trackingFactory{}
	acc, err := seederflow.NewRebindingAcceptor(factory.build())
	require.NoError(t, err)
	defer acc.Close()

	consumerPub, _ := genPair(t)
	_, seederPriv := genPair(t)

	require.NoError(t, acc.Bind(seederPriv, consumerPub))
	binds := factory.recorded()
	require.Len(t, binds, 1)
	require.Equal(t, ed25519.PublicKey(consumerPub), binds[0].ConsumerPub)
	require.Equal(t, ed25519.PrivateKey(seederPriv), binds[0].SeederPriv)
}

func TestRebindingAcceptor_AcceptReturnsConnsFromBoundListener(t *testing.T) {
	factory := &trackingFactory{}
	acc, err := seederflow.NewRebindingAcceptor(factory.build())
	require.NoError(t, err)
	defer acc.Close()

	consumerPub, _ := genPair(t)
	_, seederPriv := genPair(t)
	require.NoError(t, acc.Bind(seederPriv, consumerPub))

	listeners := factory.listeners()
	require.Len(t, listeners, 1)

	// Deliver a fake tunnel through the bound listener; Accept must return it.
	want := &stubTunnel{}
	listeners[0].conns <- want

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := acc.Accept(ctx)
	require.NoError(t, err)
	require.Same(t, want, got)
}

func TestRebindingAcceptor_AcceptBlocksUntilBound(t *testing.T) {
	factory := &trackingFactory{}
	acc, err := seederflow.NewRebindingAcceptor(factory.build())
	require.NoError(t, err)
	defer acc.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct {
		conn seederflow.TunnelConn
		err  error
	}, 1)
	go func() {
		conn, err := acc.Accept(ctx)
		done <- struct {
			conn seederflow.TunnelConn
			err  error
		}{conn, err}
	}()

	select {
	case <-done:
		t.Fatal("Accept must block until Bind is called")
	case <-time.After(50 * time.Millisecond):
	}

	consumerPub, _ := genPair(t)
	_, seederPriv := genPair(t)
	require.NoError(t, acc.Bind(seederPriv, consumerPub))
	want := &stubTunnel{}
	factory.listeners()[0].conns <- want

	select {
	case r := <-done:
		require.NoError(t, r.err)
		require.Same(t, want, r.conn)
	case <-time.After(time.Second):
		t.Fatal("Accept did not return after Bind + tunnel delivery")
	}
}

func TestRebindingAcceptor_RebindClosesPreviousListener(t *testing.T) {
	factory := &trackingFactory{}
	acc, err := seederflow.NewRebindingAcceptor(factory.build())
	require.NoError(t, err)
	defer acc.Close()

	// First binding.
	consumerPub1, _ := genPair(t)
	_, seederPriv1 := genPair(t)
	require.NoError(t, acc.Bind(seederPriv1, consumerPub1))
	first := factory.listeners()[0]
	require.False(t, first.closed.Load())

	// Second binding supersedes the first.
	consumerPub2, _ := genPair(t)
	_, seederPriv2 := genPair(t)
	require.NoError(t, acc.Bind(seederPriv2, consumerPub2))

	require.True(t, first.closed.Load(), "previous listener must be closed on rebind")
	listeners := factory.listeners()
	require.Len(t, listeners, 2)
	require.False(t, listeners[1].closed.Load(), "new listener stays open")
	require.Equal(t, ed25519.PublicKey(consumerPub2), listeners[1].consumerPub)
}

func TestRebindingAcceptor_CloseShutsDownActiveListener(t *testing.T) {
	factory := &trackingFactory{}
	acc, err := seederflow.NewRebindingAcceptor(factory.build())
	require.NoError(t, err)

	consumerPub, _ := genPair(t)
	_, seederPriv := genPair(t)
	require.NoError(t, acc.Bind(seederPriv, consumerPub))

	require.NoError(t, acc.Close())
	require.True(t, factory.listeners()[0].closed.Load())

	// Subsequent Bind on a closed acceptor must fail closed (no listener leak).
	require.Error(t, acc.Bind(seederPriv, consumerPub))
}

func TestRebindingAcceptor_FactoryErrorReturnedFromBind(t *testing.T) {
	factory := &trackingFactory{createErr: errors.New("listen failed")}
	acc, err := seederflow.NewRebindingAcceptor(factory.build())
	require.NoError(t, err)
	defer acc.Close()

	consumerPub, _ := genPair(t)
	_, seederPriv := genPair(t)
	require.Error(t, acc.Bind(seederPriv, consumerPub))
}

func TestRebindingAcceptor_MismatchedDialFailsClosed(t *testing.T) {
	// Models the scenario where the consumer dials with a different
	// ephemeral than the one bound: tunnel.Listen's TLS pinning would
	// reject the handshake with a peer-pin error. The fake listener's
	// Accept returns the same error, and the acceptor propagates it.
	factory := &trackingFactory{}
	acc, err := seederflow.NewRebindingAcceptor(factory.build())
	require.NoError(t, err)
	defer acc.Close()

	consumerPub, _ := genPair(t)
	_, seederPriv := genPair(t)
	require.NoError(t, acc.Bind(seederPriv, consumerPub))

	listeners := factory.listeners()
	require.Len(t, listeners, 1)
	pinErr := errors.New("peer pin mismatch")
	listeners[0].conns <- &errTunnel{err: pinErr}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, err := acc.Accept(ctx)
	require.NoError(t, err, "Accept itself succeeds; the per-tunnel error surfaces on read")
	require.NotNil(t, conn)
	body, readErr := conn.ReadRequest()
	require.Error(t, readErr)
	require.Nil(t, body)
	require.ErrorIs(t, readErr, pinErr)
}

// errTunnel is a TunnelConn that returns err on every read/write attempt.
type errTunnel struct {
	err error
}

func (e *errTunnel) ReadRequest() ([]byte, error) { return nil, e.err }
func (e *errTunnel) SendOK() error                { return e.err }
func (e *errTunnel) ResponseWriter() io.Writer    { return nopWriter{} }
func (e *errTunnel) SendError(string) error       { return e.err }
func (e *errTunnel) CloseWrite() error            { return nil }
func (e *errTunnel) Close() error                 { return nil }

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

package federation_test

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/federation"
)

// fakeDialerTransport implements just enough of federation.Transport to
// drive Dialer tests. Each Dial call returns whatever's at the head of
// the script queue.
type fakeDialerTransport struct {
	mu     sync.Mutex
	script []func() (federation.PeerConn, error)
	calls  int32
}

func (f *fakeDialerTransport) Dial(_ context.Context, _ string, _ ed25519.PublicKey) (federation.PeerConn, error) {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.script) == 0 {
		return nil, errors.New("script exhausted")
	}
	step := f.script[0]
	f.script = f.script[1:]
	return step()
}
func (*fakeDialerTransport) Listen(context.Context, func(federation.PeerConn)) error { return nil }
func (*fakeDialerTransport) Close() error                                            { return nil }

// fakeDialerConn is a no-op PeerConn used by the dialer tests.
type fakeDialerConn struct{ done chan struct{} }

func newFakeDialerConn() *fakeDialerConn { return &fakeDialerConn{done: make(chan struct{})} }

func (*fakeDialerConn) Send(context.Context, []byte) error   { return nil }
func (*fakeDialerConn) Recv(context.Context) ([]byte, error) { return nil, federation.ErrPeerClosed }
func (*fakeDialerConn) RemoteAddr() string                   { return "stub" }
func (*fakeDialerConn) RemotePub() ed25519.PublicKey         { return make(ed25519.PublicKey, 32) }
func (s *fakeDialerConn) Close() error                       { close(s.done); return nil }

func TestDialer_BackoffProgression_FirstFailureWaitsThenSucceeds(t *testing.T) {
	t.Parallel()
	_, priv, _ := ed25519.GenerateKey(crand.Reader)

	conn := newFakeDialerConn()
	transport := &fakeDialerTransport{script: []func() (federation.PeerConn, error){
		func() (federation.PeerConn, error) { return nil, errors.New("dial fail #1") },
		func() (federation.PeerConn, error) { return conn, nil },
	}}

	connectedCh := make(chan struct{}, 1)
	d := &federation.Dialer{
		Transport: transport,
		Peer: federation.AllowlistedPeer{
			TrackerID: ids.TrackerID{},
			PubKey:    make(ed25519.PublicKey, 32),
			Addr:      "127.0.0.1:1",
		},
		MyTrackerID: ids.TrackerID{},
		MyPriv:      priv,
		HandshakeTO: 50 * time.Millisecond,
		BackoffBase: 25 * time.Millisecond,
		BackoffMax:  100 * time.Millisecond,
		Now:         time.Now,
		OnConnected: func(c federation.PeerConn) {
			connectedCh <- struct{}{}
			<-c.(*fakeDialerConn).done
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	go d.Run(ctx)

	select {
	case <-connectedCh:
	case <-ctx.Done():
		t.Fatalf("dialer never connected; calls=%d", atomic.LoadInt32(&transport.calls))
	}
	if got := atomic.LoadInt32(&transport.calls); got != 2 {
		t.Fatalf("dial calls = %d, want 2 (one fail then success)", got)
	}
	_ = conn.Close()
	cancel()
}

func TestDialer_CtxCancel_MidSleep_Exits(t *testing.T) {
	t.Parallel()
	_, priv, _ := ed25519.GenerateKey(crand.Reader)
	transport := &fakeDialerTransport{script: []func() (federation.PeerConn, error){
		func() (federation.PeerConn, error) { return nil, errors.New("fail") },
	}}

	d := &federation.Dialer{
		Transport:   transport,
		Peer:        federation.AllowlistedPeer{PubKey: make(ed25519.PublicKey, 32), Addr: "127.0.0.1:1"},
		MyPriv:      priv,
		HandshakeTO: 50 * time.Millisecond,
		BackoffBase: 5 * time.Second,
		BackoffMax:  5 * time.Second,
		Now:         time.Now,
		OnConnected: func(federation.PeerConn) {},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&transport.calls) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dialer did not exit after ctx cancel")
	}
}

package federation_test

import (
	"context"
	"crypto/sha256"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/federation"
)

// fakePeer implements federation.SendOnlyPeer for gossip tests.
type fakePeer struct {
	id     ids.TrackerID
	mu     sync.Mutex
	frames [][]byte
	fail   bool
}

func (f *fakePeer) ID() ids.TrackerID { return f.id }
func (f *fakePeer) Send(_ context.Context, frame []byte) error {
	if f.fail {
		return federation.ErrPeerClosed
	}
	f.mu.Lock()
	f.frames = append(f.frames, append([]byte(nil), frame...))
	f.mu.Unlock()
	return nil
}

func (f *fakePeer) Frames() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.frames...)
}

func TestGossip_ForwardSkipsExclude(t *testing.T) {
	t.Parallel()
	a := &fakePeer{id: ids.TrackerID{1}}
	b := &fakePeer{id: ids.TrackerID{2}}
	c := &fakePeer{id: ids.TrackerID{3}}
	src := newPeerCfg(t)
	g := federation.NewGossipForTest([]federation.SendOnlyPeer{a, b, c}, src.priv, src.id)

	if err := g.Forward(context.Background(), fed.Kind_KIND_PING, []byte{1, 2, 3}, &b.id); err != nil {
		t.Fatal(err)
	}
	if got := len(a.Frames()); got != 1 {
		t.Fatalf("a.frames = %d", got)
	}
	if got := len(b.Frames()); got != 0 {
		t.Fatalf("b.frames = %d (should be excluded)", got)
	}
	if got := len(c.Frames()); got != 1 {
		t.Fatalf("c.frames = %d", got)
	}
}

func TestGossip_ForwardSucceedsDespiteOnePeerError(t *testing.T) {
	t.Parallel()
	a := &fakePeer{id: ids.TrackerID{1}}
	b := &fakePeer{id: ids.TrackerID{2}, fail: true}
	src := newPeerCfg(t)
	g := federation.NewGossipForTest([]federation.SendOnlyPeer{a, b}, src.priv, src.id)
	if err := g.Forward(context.Background(), fed.Kind_KIND_PING, []byte{1}, nil); err != nil {
		t.Fatal(err)
	}
	if len(a.Frames()) != 1 {
		t.Fatal("a should have received the frame")
	}
}

func TestGossip_ForwardMessageIDIsStable(t *testing.T) {
	t.Parallel()
	a := &fakePeer{id: ids.TrackerID{1}}
	src := newPeerCfg(t)
	g := federation.NewGossipForTest([]federation.SendOnlyPeer{a}, src.priv, src.id)
	payload := []byte{9, 9, 9}
	expectedID := sha256.Sum256(payload)
	got := atomic.Pointer[[32]byte]{}
	g = g.WithObserver(func(mid [32]byte, _ fed.Kind) {
		copy := mid
		got.Store(&copy)
	})
	_ = g.Forward(context.Background(), fed.Kind_KIND_PING, payload, nil)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got.Load() != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got.Load() == nil || *got.Load() != expectedID {
		t.Fatalf("observed message_id = %v want %x", got.Load(), expectedID)
	}
}

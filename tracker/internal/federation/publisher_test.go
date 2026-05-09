package federation_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/tracker/internal/federation"
)

type fakeRootSrc struct {
	root, sig []byte
	ok        bool
	err       error
	hour      uint64
	called    atomic.Int32
}

func (f *fakeRootSrc) ReadyRoot(_ context.Context, h uint64) ([]byte, []byte, bool, error) {
	f.called.Add(1)
	f.hour = h
	return f.root, f.sig, f.ok, f.err
}

func TestPublisher_Tick_PublishesReadyRoot(t *testing.T) {
	t.Parallel()
	src := &fakeRootSrc{root: b(32, 7), sig: b(64, 8), ok: true}
	pubs := int32(0)
	fwd := func(_ context.Context, kind fed.Kind, _ []byte) {
		if kind == fed.Kind_KIND_ROOT_ATTESTATION {
			atomic.AddInt32(&pubs, 1)
		}
	}
	cli := newPeerCfg(t)
	pub := federation.NewPublisher(src, fwd, cli.id)

	if err := pub.PublishHour(context.Background(), 42); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := atomic.LoadInt32(&pubs); got != 1 {
		t.Fatalf("pubs = %d", got)
	}
}

func TestPublisher_Tick_SkipsWhenNotReady(t *testing.T) {
	t.Parallel()
	src := &fakeRootSrc{ok: false}
	pubs := int32(0)
	fwd := func(_ context.Context, kind fed.Kind, _ []byte) {
		if kind == fed.Kind_KIND_ROOT_ATTESTATION {
			atomic.AddInt32(&pubs, 1)
		}
	}
	cli := newPeerCfg(t)
	pub := federation.NewPublisher(src, fwd, cli.id)
	if err := pub.PublishHour(context.Background(), 42); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := atomic.LoadInt32(&pubs); got != 0 {
		t.Fatalf("pubs = %d (expected 0; not ready)", got)
	}
}

func TestPublisher_Tick_PropagatesSourceError(t *testing.T) {
	t.Parallel()
	want := errors.New("boom")
	src := &fakeRootSrc{err: want}
	cli := newPeerCfg(t)
	pub := federation.NewPublisher(src, func(context.Context, fed.Kind, []byte) {}, cli.id)
	if err := pub.PublishHour(context.Background(), 42); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

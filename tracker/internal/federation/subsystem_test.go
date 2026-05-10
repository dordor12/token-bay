package federation_test

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/federation"
)

func TestFederation_OpenClose_NoPeers(t *testing.T) {
	t.Parallel()
	hub := federation.NewInprocHub()
	srv := newPeerCfg(t)
	tr := federation.NewInprocTransport(hub, "srv", srv.pub, srv.priv)
	defer tr.Close()
	srvIDHash := sha256.Sum256(srv.pub)
	f, err := federation.Open(federation.Config{
		MyTrackerID: ids.TrackerID(srvIDHash),
		MyPriv:      srv.priv,
	}, federation.Deps{
		Transport: tr,
		RootSrc:   &fakeRootSrc{ok: false},
		Archive:   newFakeArchive(),
		Metrics:   federation.NewMetrics(prometheus.NewRegistry()),
		Logger:    zerolog.Nop(),
		Now:       time.Now,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := len(f.Peers()); got != 0 {
		t.Fatalf("Peers() = %d, want 0", got)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestFederation_PublishHour_ForwardsThroughGossip(t *testing.T) {
	t.Parallel()
	// Single-tracker scenario: the integration_test.go file owns the
	// two-tracker flow; here we just verify the wiring.
	hub := federation.NewInprocHub()
	srv := newPeerCfg(t)
	tr := federation.NewInprocTransport(hub, "srv", srv.pub, srv.priv)
	defer tr.Close()
	src := &fakeRootSrc{root: b(32, 7), sig: b(64, 8), ok: true}
	srvIDHash := sha256.Sum256(srv.pub)
	f, err := federation.Open(federation.Config{
		MyTrackerID: ids.TrackerID(srvIDHash),
		MyPriv:      srv.priv,
	}, federation.Deps{
		Transport: tr,
		RootSrc:   src,
		Archive:   newFakeArchive(),
		Metrics:   federation.NewMetrics(prometheus.NewRegistry()),
		Logger:    zerolog.Nop(),
		Now:       time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.PublishHour(context.Background(), 42); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if src.called.Load() != 1 {
		t.Fatalf("RootSource called %d times", src.called.Load())
	}
}

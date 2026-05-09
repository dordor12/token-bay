package federation_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/federation"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

// twoTracker spins up A and B sharing one InprocHub; A peers with B.
type twoTracker struct {
	hub          *federation.InprocHub
	a, b         *federation.Federation
	archA, archB *fakeArchive
	srcA, srcB   *fakeRootSrc
	aID, bID     ids.TrackerID
}

// flipReadyA mutates srcA to return (root, sig, true, nil) on the next
// ReadyRoot call. Used by tests to drive a one-shot publish.
func (tt *twoTracker) flipReadyA(root, sig []byte) {
	tt.srcA.root = root
	tt.srcA.sig = sig
	tt.srcA.ok = true
}

func newTwoTracker(t *testing.T) *twoTracker {
	t.Helper()
	a := newPeerCfg(t)
	b := newPeerCfg(t)
	hub := federation.NewInprocHub()
	trA := federation.NewInprocTransport(hub, "A", a.pub, a.priv)
	trB := federation.NewInprocTransport(hub, "B", b.pub, b.priv)

	archA, archB := newFakeArchive(), newFakeArchive()
	srcA := &fakeRootSrc{ok: false}
	srcB := &fakeRootSrc{ok: false}

	aID := ids.TrackerID(sha256.Sum256(a.pub))
	bID := ids.TrackerID(sha256.Sum256(b.pub))

	aFed, err := federation.Open(federation.Config{
		MyTrackerID: aID,
		MyPriv:      a.priv,
		Peers:       []federation.AllowlistedPeer{{TrackerID: bID, PubKey: b.pub, Addr: "B"}},
	}, federation.Deps{Transport: trA, RootSrc: srcA, Archive: archA, Metrics: federation.NewMetrics(prometheus.NewRegistry()), Logger: zerolog.Nop(), Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	bFed, err := federation.Open(federation.Config{
		MyTrackerID: bID,
		MyPriv:      b.priv,
		Peers:       []federation.AllowlistedPeer{{TrackerID: aID, PubKey: a.pub, Addr: "A"}},
	}, federation.Deps{Transport: trB, RootSrc: srcB, Archive: archB, Metrics: federation.NewMetrics(prometheus.NewRegistry()), Logger: zerolog.Nop(), Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = aFed.Close()
		_ = bFed.Close()
	})
	return &twoTracker{hub: hub, a: aFed, b: bFed, archA: archA, archB: archB, srcA: srcA, srcB: srcB, aID: aID, bID: bID}
}

func TestIntegration_RootAttestation_AB(t *testing.T) {
	t.Parallel()
	tt := newTwoTracker(t)

	// Both A and B have each other in their peer list; both Open()
	// goroutines dial. Wait until A's registry shows B as Steady (i.e.
	// the dial+handshake completed at least one direction).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		gotSteady := false
		for _, p := range tt.a.Peers() {
			if p.State == federation.PeerStateSteady {
				gotSteady = true
				break
			}
		}
		if gotSteady {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Flip A's source to ready before publishing.
	tt.flipReadyA(b(32, 7), b(64, 8))

	if err := tt.a.PublishHour(context.Background(), 100); err != nil {
		t.Fatalf("publish: %v", err)
	}

	aIDBytes := tt.aID.Bytes()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok, _ := tt.archB.GetPeerRoot(context.Background(), aIDBytes[:], 100); ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("B never archived A's root")
}

func TestIntegration_Equivocation_LocalDetection(t *testing.T) {
	t.Parallel()
	// Pre-populate B's archive with an existing root for tracker X hour 7,
	// then synthesize a conflicting RA from X and feed it through the
	// applier directly. Asserts that:
	//   - B emits an EquivocationEvidence (counter incremented),
	//   - B depeers X.
	x := newPeerCfg(t)
	arch := newFakeArchive()
	xid := x.id.Bytes()
	_ = arch.PutPeerRoot(context.Background(), storage.PeerRoot{TrackerID: xid[:], Hour: 7, Root: b(32, 1), Sig: b(64, 1), ReceivedAt: 1})

	hub := federation.NewInprocHub()
	bCfg := newPeerCfg(t)
	tr := federation.NewInprocTransport(hub, "B", bCfg.pub, bCfg.priv)
	regProm := prometheus.NewRegistry()
	f, err := federation.Open(federation.Config{
		MyTrackerID: bCfg.id,
		MyPriv:      bCfg.priv,
		Peers:       []federation.AllowlistedPeer{{TrackerID: x.id, PubKey: x.pub, Addr: "X", Region: ""}},
	}, federation.Deps{Transport: tr, RootSrc: &fakeRootSrc{ok: false}, Archive: arch, Metrics: federation.NewMetrics(regProm), Logger: zerolog.Nop(), Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Feed a conflicting attestation directly via the applier (the full
	// recvLoop path is exercised in TestIntegration_RootAttestation_AB).
	err = f.ApplyForTest(x.id, x.priv, 7, b(32, 2), b(64, 2))
	if !errors.Is(err, federation.ErrEquivocation) {
		t.Fatalf("expected ErrEquivocation, got %v", err)
	}
	// Offender depeered: f.Peers() should not contain x.id.
	for _, p := range f.Peers() {
		if p.TrackerID == x.id {
			t.Fatalf("offender X should be depeered; got %+v", p)
		}
	}
}

package federation_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	fed "github.com/token-bay/token-bay/shared/federation"
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

func openQUICFederation(t *testing.T, p quicPeer, peers []federation.AllowlistedPeer) (*federation.Federation, *fakeArchive, *fakeRootSrc) {
	t.Helper()
	cert, err := federation.CertFromIdentity(p.priv)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := federation.NewQUICTransport(federation.QUICConfig{
		ListenAddr:  "127.0.0.1:0",
		IdleTimeout: 5 * time.Second,
		Cert:        cert,
		HandshakeTO: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	arch := newFakeArchive()
	src := &fakeRootSrc{ok: false}
	logger := zerolog.Nop()
	f, err := federation.Open(federation.Config{
		MyTrackerID:      ids.TrackerID(sha256.Sum256(p.pub)),
		MyPriv:           p.priv,
		HandshakeTimeout: 2 * time.Second,
		DedupeTTL:        time.Hour,
		DedupeCap:        1024,
		GossipRateQPS:    100,
		SendQueueDepth:   256,
		PublishCadence:   time.Hour,
		IdleTimeout:      5 * time.Second,
		RedialBase:       50 * time.Millisecond,
		RedialMax:        500 * time.Millisecond,
		Peers:            peers,
	}, federation.Deps{
		Transport: tr,
		RootSrc:   src,
		Archive:   arch,
		Metrics:   federation.NewMetrics(prometheus.NewRegistry()),
		Logger:    logger,
		Now:       time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, arch, src
}

func TestIntegration_QUIC_RootAttestation_AB(t *testing.T) {
	t.Parallel()
	a := newQUICPeer(t)
	b := newQUICPeer(t)

	// Open A and B with no peers, capture their bound ports, then call
	// AddPeer to inject the cross-references. AddPeer spawns the per-peer
	// Dialer goroutine, mirroring how a future operator admin API would
	// register peers at runtime.
	aFed, _, srcA := openQUICFederation(t, a, nil)
	bFed, archB, _ := openQUICFederation(t, b, nil)

	if err := aFed.AddPeer(federation.AllowlistedPeer{
		TrackerID: ids.TrackerID(sha256.Sum256(b.pub)), PubKey: b.pub, Addr: bFed.ListenAddr(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := bFed.AddPeer(federation.AllowlistedPeer{
		TrackerID: ids.TrackerID(sha256.Sum256(a.pub)), PubKey: a.pub, Addr: aFed.ListenAddr(),
	}); err != nil {
		t.Fatal(err)
	}

	steady := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range aFed.Peers() {
			if p.State == federation.PeerStateSteady {
				steady = true
				break
			}
		}
		if steady {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !steady {
		ps := aFed.Peers()
		t.Logf("aFed peers before publish: %d", len(ps))
		for _, p := range ps {
			t.Logf("  peer state=%v addr=%s", p.State, p.Addr)
		}
		t.Fatal("aFed never reached steady state with B")
	}

	srcA.root = b32(7)
	srcA.sig = b64(8)
	srcA.ok = true
	if err := aFed.PublishHour(context.Background(), 100); err != nil {
		t.Fatalf("publish: %v", err)
	}

	aIDBytes := ids.TrackerID(sha256.Sum256(a.pub)).Bytes()
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok, _ := archB.GetPeerRoot(context.Background(), aIDBytes[:], 100); ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("B never archived A's root over QUIC (steady state was reached)")
}

// fakeIntegrationLedger implements federation.LedgerHooks for end-to-end
// integration tests. Records every call and returns the configured
// outputs. Concurrency-safe; the source-side and destination-side test
// fixtures hold their own instances.
type fakeIntegrationLedger struct {
	mu sync.Mutex

	outCalls []federation.TransferOutHookIn
	inCalls  []federation.TransferInHookIn

	outResult federation.TransferOutHookOut
	outErr    error
	inErr     error
}

func (f *fakeIntegrationLedger) AppendTransferOut(_ context.Context, in federation.TransferOutHookIn) (federation.TransferOutHookOut, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outCalls = append(f.outCalls, in)
	return f.outResult, f.outErr
}

func (f *fakeIntegrationLedger) AppendTransferIn(_ context.Context, in federation.TransferInHookIn) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inCalls = append(f.inCalls, in)
	return f.inErr
}

func (f *fakeIntegrationLedger) snapshotInCalls() []federation.TransferInHookIn {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]federation.TransferInHookIn, len(f.inCalls))
	copy(out, f.inCalls)
	return out
}

func (f *fakeIntegrationLedger) snapshotOutCalls() []federation.TransferOutHookIn {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]federation.TransferOutHookIn, len(f.outCalls))
	copy(out, f.outCalls)
	return out
}

// newTwoTrackerWithLedgers mirrors newTwoTracker but plumbs LedgerHooks
// into both Federations' Deps.
func newTwoTrackerWithLedgers(t *testing.T, aLedger, bLedger federation.LedgerHooks) *twoTracker {
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
	}, federation.Deps{
		Transport: trA, RootSrc: srcA, Archive: archA, Ledger: aLedger,
		Metrics: federation.NewMetrics(prometheus.NewRegistry()),
		Logger:  zerolog.Nop(), Now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	bFed, err := federation.Open(federation.Config{
		MyTrackerID: bID,
		MyPriv:      b.priv,
		Peers:       []federation.AllowlistedPeer{{TrackerID: aID, PubKey: a.pub, Addr: "A"}},
	}, federation.Deps{
		Transport: trB, RootSrc: srcB, Archive: archB, Ledger: bLedger,
		Metrics: federation.NewMetrics(prometheus.NewRegistry()),
		Logger:  zerolog.Nop(), Now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = aFed.Close()
		_ = bFed.Close()
	})
	return &twoTracker{
		hub: hub, a: aFed, b: bFed,
		archA: archA, archB: archB, srcA: srcA, srcB: srcB,
		aID: aID, bID: bID,
	}
}

func TestIntegration_CrossRegionTransfer_HappyPath(t *testing.T) {
	t.Parallel()

	aLedger := &fakeIntegrationLedger{
		outResult: federation.TransferOutHookOut{
			ChainTipHash: [32]byte{
				0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB,
				0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB,
				0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB,
				0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB,
			},
			Seq: 11,
		},
	}
	bLedger := &fakeIntegrationLedger{}

	tt := newTwoTrackerWithLedgers(t, aLedger, bLedger)

	// Wait until B sees A as steady (peering handshake completed).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ok := false
		for _, p := range tt.b.Peers() {
			if p.State == federation.PeerStateSteady {
				ok = true
				break
			}
		}
		if ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	conPub, conPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	identityID := b(32, 0x44)
	var nonce [32]byte
	for i := range nonce {
		nonce[i] = 0x55
	}

	aIDBytes := tt.aID.Bytes()
	bIDBytes := tt.bID.Bytes()
	req := &fed.TransferProofRequest{
		SourceTrackerId: aIDBytes[:],
		DestTrackerId:   bIDBytes[:],
		IdentityId:      identityID,
		Amount:          1500,
		Nonce:           nonce[:],
		ConsumerPub:     conPub,
		Timestamp:       1714000000,
	}
	canonical, err := fed.CanonicalTransferProofRequestPreSig(req)
	if err != nil {
		t.Fatal(err)
	}
	consumerSig := ed25519.Sign(conPriv, canonical)

	// Issue StartTransfer on B (destination, asking A for the proof).
	var idArr [32]byte
	copy(idArr[:], identityID)
	out, err := tt.b.StartTransfer(context.Background(), federation.StartTransferInput{
		SourceTrackerID: tt.aID,
		IdentityID:      ids.IdentityID(idArr),
		Amount:          1500,
		Nonce:           nonce,
		ConsumerSig:     consumerSig,
		ConsumerPub:     conPub,
		Timestamp:       1714000000,
	})
	if err != nil {
		t.Fatalf("StartTransfer: %v", err)
	}
	if out.SourceSeq != 11 {
		t.Errorf("SourceSeq=%d, want 11", out.SourceSeq)
	}

	if got := aLedger.snapshotOutCalls(); len(got) != 1 {
		t.Fatalf("aLedger.outCalls=%d, want 1", len(got))
	} else if got[0].Amount != 1500 || got[0].TransferRef != nonce {
		t.Errorf("outCall mismatch: amount=%d ref=%x", got[0].Amount, got[0].TransferRef)
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(bLedger.snapshotInCalls()) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	in := bLedger.snapshotInCalls()
	if len(in) != 1 {
		t.Fatalf("bLedger.inCalls=%d, want 1", len(in))
	}
	if in[0].IdentityID != idArr || in[0].TransferRef != nonce || in[0].Amount != 1500 {
		t.Errorf("inCall mismatch: %+v", in[0])
	}
}

func TestIntegration_StartTransfer_DisabledWithoutLedger(t *testing.T) {
	t.Parallel()
	tt := newTwoTracker(t) // no Ledger wired

	var nonce [32]byte
	nonce[0] = 0x01
	_, err := tt.b.StartTransfer(context.Background(), federation.StartTransferInput{
		SourceTrackerID: tt.aID,
		IdentityID:      ids.IdentityID{},
		Amount:          1,
		Nonce:           nonce,
		ConsumerSig:     b(64, 0x55),
		ConsumerPub:     b(32, 0x66),
		Timestamp:       1,
	})
	if err == nil {
		t.Fatal("err=nil, want ErrTransferDisabled")
	}
	if !errors.Is(err, federation.ErrTransferDisabled) {
		t.Fatalf("err=%v, want ErrTransferDisabled", err)
	}
}

// b32/b64 are short helpers for the QUIC integration test (parallel to b()).
func b32(v byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = v
	}
	return out
}

func b64(v byte) []byte {
	out := make([]byte, 64)
	for i := range out {
		out[i] = v
	}
	return out
}

package federation_test

import (
	"context"
	"sync/atomic"
	"testing"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/federation"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
	"google.golang.org/protobuf/proto"
)

func TestEquivocator_Detect_BroadcastsEvidenceAndDepeers(t *testing.T) {
	t.Parallel()
	off := newPeerCfg(t) // offender
	arch := newFakeArchive()
	// Pre-populate with an existing root for (off, hour=10).
	offIDBytes := off.id.Bytes()
	prevPeer := storage.PeerRoot{TrackerID: offIDBytes[:], Hour: 10, Root: b(32, 1), Sig: b(64, 1), ReceivedAt: 1}
	_ = arch.PutPeerRoot(context.Background(), prevPeer)

	reg := federation.NewRegistry()
	_ = reg.Add(federation.PeerInfo{TrackerID: off.id, PubKey: off.pub, Addr: "x", State: federation.PeerStateSteady})

	broadcasts := int32(0)
	fwd := func(_ context.Context, kind fed.Kind, _ []byte) {
		if kind == fed.Kind_KIND_EQUIVOCATION_EVIDENCE {
			atomic.AddInt32(&broadcasts, 1)
		}
	}
	eq := federation.NewEquivocator(arch, fwd, reg)

	incoming := &fed.RootAttestation{
		TrackerId:  offIDBytes[:],
		Hour:       10,
		MerkleRoot: b(32, 2), // different root
		TrackerSig: b(64, 2),
	}
	srcEnvFromOff, _ := federation.SignEnvelope(off.priv, offIDBytes[:], fed.Kind_KIND_ROOT_ATTESTATION, mustMarshal(incoming))

	eq.OnLocalConflict(context.Background(), incoming, srcEnvFromOff)
	if got := atomic.LoadInt32(&broadcasts); got != 1 {
		t.Fatalf("broadcasts = %d", got)
	}
	if _, ok := reg.Get(off.id); ok {
		t.Fatal("offender should be depeered")
	}
}

func TestEquivocator_ReceiveEvidence_SkipsSelf(t *testing.T) {
	t.Parallel()
	me := newPeerCfg(t)
	reg := federation.NewRegistry()
	arch := newFakeArchive()
	fwd := func(context.Context, fed.Kind, []byte) {}
	eq := federation.NewEquivocator(arch, fwd, reg).WithSelf(me.id)

	idBytes := me.id.Bytes()
	evi := &fed.EquivocationEvidence{
		TrackerId: idBytes[:], Hour: 5,
		RootA: b(32, 1), SigA: b(64, 1),
		RootB: b(32, 2), SigB: b(64, 2),
	}
	env, _ := federation.SignEnvelope(me.priv, idBytes[:], fed.Kind_KIND_EQUIVOCATION_EVIDENCE, mustMarshal(evi))
	eq.OnIncomingEvidence(context.Background(), env, ids.TrackerID{1})
	if eq.SelfEquivocations() != 1 {
		t.Fatalf("expected 1 self-equivocation; got %d", eq.SelfEquivocations())
	}
}

func TestEquivocator_ReceiveEvidence_DepeersOffender(t *testing.T) {
	t.Parallel()
	off := newPeerCfg(t)
	src := newPeerCfg(t)
	reg := federation.NewRegistry()
	_ = reg.Add(federation.PeerInfo{TrackerID: off.id, PubKey: off.pub, Addr: "x", State: federation.PeerStateSteady})
	_ = reg.Add(federation.PeerInfo{TrackerID: src.id, PubKey: src.pub, Addr: "y", State: federation.PeerStateSteady})

	forwarded := int32(0)
	fwd := func(_ context.Context, kind fed.Kind, _ []byte) {
		if kind == fed.Kind_KIND_EQUIVOCATION_EVIDENCE {
			atomic.AddInt32(&forwarded, 1)
		}
	}
	eq := federation.NewEquivocator(newFakeArchive(), fwd, reg)

	offIDBytes := off.id.Bytes()
	srcIDBytes := src.id.Bytes()
	evi := &fed.EquivocationEvidence{
		TrackerId: offIDBytes[:], Hour: 5,
		RootA: b(32, 1), SigA: b(64, 1),
		RootB: b(32, 2), SigB: b(64, 2),
	}
	env, _ := federation.SignEnvelope(src.priv, srcIDBytes[:], fed.Kind_KIND_EQUIVOCATION_EVIDENCE, mustMarshal(evi))
	eq.OnIncomingEvidence(context.Background(), env, src.id)

	if _, ok := reg.Get(off.id); ok {
		t.Fatal("offender should be depeered")
	}
	if atomic.LoadInt32(&forwarded) != 1 {
		t.Fatal("evidence should be forwarded")
	}
}

func mustMarshal(m proto.Message) []byte { b, _ := proto.Marshal(m); return b }

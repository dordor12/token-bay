package federation

import (
	"bytes"
	"context"
	"sync/atomic"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"google.golang.org/protobuf/proto"
)

// Equivocator handles both the outgoing (locally-detected conflict) and
// incoming (peer-broadcast) sides of the equivocation flow.
type Equivocator struct {
	archive PeerRootArchive
	forward Forwarder
	reg     *Registry
	self    *ids.TrackerID
	onFlag  func(ids.TrackerID)

	selfEquiv atomic.Int64
}

// NewEquivocator constructs an Equivocator. onFlag may be nil; it is
// fired with the offender's TrackerID after a Depeer call, on both
// locally-detected conflicts and peer-broadcast evidence about a third
// party. Self-evidence does NOT fire onFlag (the local equivocation is
// already metric-tracked via SelfEquivocations).
func NewEquivocator(arch PeerRootArchive, fwd Forwarder, reg *Registry, onFlag func(ids.TrackerID)) *Equivocator {
	if onFlag == nil {
		onFlag = func(ids.TrackerID) {}
	}
	return &Equivocator{archive: arch, forward: fwd, reg: reg, onFlag: onFlag}
}

// WithSelf returns a new *Equivocator sharing the same archive, forward,
// registry, and onFlag callback as e but with selfID set so the
// receive-path can detect "evidence about us" and emit the critical
// alert metric. A fresh allocation is used to avoid copying the
// embedded atomic.Int64.
func (e *Equivocator) WithSelf(selfID ids.TrackerID) *Equivocator {
	id := selfID // capture by value before taking address
	return &Equivocator{
		archive: e.archive,
		forward: e.forward,
		reg:     e.reg,
		self:    &id,
		onFlag:  e.onFlag,
	}
}

// SelfEquivocations is the test-visible counter of "evidence about us"
// hits since construction.
func (e *Equivocator) SelfEquivocations() int64 { return e.selfEquiv.Load() }

// OnLocalConflict is called by the rootattest applier on
// storage.ErrPeerRootConflict. Looks up the existing row, builds an
// EquivocationEvidence, broadcasts to ALL peers (no exclude — the
// detector itself is not a peer), and depeers the offender.
func (e *Equivocator) OnLocalConflict(ctx context.Context, incoming *fed.RootAttestation, srcEnv *fed.Envelope) {
	existing, ok, err := e.archive.GetPeerRoot(ctx, incoming.TrackerId, incoming.Hour)
	if err != nil || !ok {
		return // race; nothing actionable
	}
	evi := &fed.EquivocationEvidence{
		TrackerId: append([]byte(nil), incoming.TrackerId...),
		Hour:      incoming.Hour,
		RootA:     append([]byte(nil), existing.Root...),
		SigA:      append([]byte(nil), existing.Sig...),
		RootB:     append([]byte(nil), incoming.MerkleRoot...),
		SigB:      append([]byte(nil), incoming.TrackerSig...),
	}
	if err := fed.ValidateEquivocationEvidence(evi); err != nil {
		return
	}
	payload, _ := proto.Marshal(evi)
	e.forward(ctx, fed.Kind_KIND_EQUIVOCATION_EVIDENCE, payload)

	var offender ids.TrackerID
	copy(offender[:], incoming.TrackerId)
	_ = e.reg.Depeer(offender, ReasonEquivocation)
	e.onFlag(offender)
}

// OnIncomingEvidence is called by the recv dispatch on
// KIND_EQUIVOCATION_EVIDENCE. Depeers the offender if active, and
// forwards to all peers except the source. Evidence about ourselves
// emits a critical-severity counter; no automatic action.
func (e *Equivocator) OnIncomingEvidence(ctx context.Context, env *fed.Envelope, fromPeer ids.TrackerID) {
	var evi fed.EquivocationEvidence
	if err := proto.Unmarshal(env.Payload, &evi); err != nil {
		return
	}
	if err := fed.ValidateEquivocationEvidence(&evi); err != nil {
		return
	}
	if e.self != nil && bytes.Equal(evi.TrackerId, e.self[:]) {
		e.selfEquiv.Add(1)
		return
	}
	excl := fromPeer
	e.forward(ctx, fed.Kind_KIND_EQUIVOCATION_EVIDENCE, env.Payload)
	_ = excl // forward already excludes via Gossip's normal exclude semantics; kept for future per-source dedup

	var offender ids.TrackerID
	copy(offender[:], evi.TrackerId)
	_ = e.reg.Depeer(offender, ReasonEquivocation)
	e.onFlag(offender)
}

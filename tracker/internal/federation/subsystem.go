package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"google.golang.org/protobuf/proto"
)

// Federation is the subsystem's public type.
type Federation struct {
	cfg Config
	dep Deps

	reg    *Registry
	dedupe *Dedupe
	gossip *Gossip
	apply  *RootAttestApplier
	equiv  *Equivocator
	pub    *Publisher

	listenCancel context.CancelFunc
	mu           sync.Mutex
	peers        map[ids.TrackerID]*Peer // active recv goroutines
	closed       bool
}

// Open constructs and starts the federation subsystem. It returns a ready
// *Federation or an error if required Deps are missing.
func Open(cfg Config, dep Deps) (*Federation, error) {
	cfg = cfg.withDefaults()
	if dep.Transport == nil {
		return nil, errors.New("federation: Transport required")
	}
	if dep.RootSrc == nil {
		return nil, errors.New("federation: RootSource required")
	}
	if dep.Archive == nil {
		return nil, errors.New("federation: Archive required")
	}
	if dep.Metrics == nil {
		return nil, errors.New("federation: Metrics required")
	}
	if dep.Now == nil {
		dep.Now = NowFromTime
	}

	reg := NewRegistry()
	dedupe := NewDedupe(cfg.DedupeTTL, cfg.DedupeCap, dep.Now)

	// Build gossip backed by registry + per-peer Send. peerSet is a
	// pointer so we can patch the *Federation back-reference after
	// constructing federation below.
	peerSet := &registryPeerSet{} // f filled in after Federation is built
	gossip := NewGossip(cfg.MyPriv, cfg.MyTrackerID, peerSet)

	// Wrap Forward into a closure that dedupes locally and counts metrics.
	forward := func(ctx context.Context, kind fed.Kind, payload []byte) {
		dedupe.Mark(sha256.Sum256(payload))
		_ = gossip.Forward(ctx, kind, payload, nil)
		dep.Metrics.FramesOut(kind.String())
		if kind == fed.Kind_KIND_ROOT_ATTESTATION {
			dep.Metrics.RootAttestationsPublished()
		}
	}

	apply := NewRootAttestApplier(dep.Archive, forward, dep.Now)
	equiv := NewEquivocator(dep.Archive, forward, reg).WithSelf(cfg.MyTrackerID)
	apply.RegisterEquivocator(equiv.OnLocalConflict)

	pub := NewPublisher(dep.RootSrc, forward, cfg.MyTrackerID)

	f := &Federation{
		cfg: cfg, dep: dep,
		reg: reg, dedupe: dedupe, gossip: gossip,
		apply: apply, equiv: equiv, pub: pub,
		peers: make(map[ids.TrackerID]*Peer),
	}
	peerSet.f = f

	// Add operator-allowlisted peers in pending state. Dialing them is
	// kicked off below; this slice initializes their entries so they
	// are visible via Peers().
	for _, p := range cfg.Peers {
		_ = reg.Add(PeerInfo{TrackerID: p.TrackerID, PubKey: p.PubKey, Addr: p.Addr, Region: p.Region, State: PeerStatePending})
	}

	// Listen on the transport. The accept callback runs the listener-side
	// handshake and, on success, attaches a Peer recv loop.
	ctx, cancel := context.WithCancel(context.Background())
	f.listenCancel = cancel
	go func() { _ = dep.Transport.Listen(ctx, f.acceptInbound) }()

	// Dial each operator-allowlisted peer in a goroutine. The dialer
	// runs the dialer-side handshake and, on success, attaches a Peer
	// recv loop. Dials that fail are logged + counted; redial is the
	// real-transport slice's concern, but for the in-process transport
	// the listener is already up by the time we dial (synchronous Listen
	// registration in the hub).
	for _, p := range cfg.Peers {
		go f.dialOutbound(ctx, p)
	}
	return f, nil
}

func (f *Federation) dialOutbound(ctx context.Context, p AllowlistedPeer) {
	conn, err := f.dep.Transport.Dial(ctx, p.Addr, p.PubKey)
	if err != nil {
		f.dep.Metrics.InvalidFrames("dial")
		return
	}
	res, err := RunHandshakeDialer(ctx, conn, f.cfg.MyTrackerID, f.cfg.MyPriv, p.TrackerID, p.PubKey, f.cfg.HandshakeTimeout)
	if err != nil {
		f.dep.Metrics.InvalidFrames("handshake")
		_ = conn.Close()
		return
	}
	f.attachPeer(res, conn)
}

// Peers is the operator-facing snapshot of all known peers.
func (f *Federation) Peers() []PeerInfo { return f.reg.All() }

// Depeer removes a peer from the active set.
func (f *Federation) Depeer(id ids.TrackerID, reason DepeerReason) error {
	f.mu.Lock()
	if p, ok := f.peers[id]; ok {
		p.Stop()
		delete(f.peers, id)
	}
	f.mu.Unlock()
	return f.reg.Depeer(id, reason)
}

// PublishHour drives a single publisher tick.
func (f *Federation) PublishHour(ctx context.Context, hour uint64) error {
	return f.pub.PublishHour(ctx, hour)
}

// Close shuts down the subsystem, stopping all peer goroutines and closing
// the transport. Safe to call more than once.
func (f *Federation) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	for _, p := range f.peers {
		p.Stop()
	}
	f.peers = nil
	f.mu.Unlock()
	if f.listenCancel != nil {
		f.listenCancel()
	}
	return f.dep.Transport.Close()
}

func (f *Federation) acceptInbound(c PeerConn) {
	expected := map[ids.TrackerID]ed25519.PublicKey{}
	for _, p := range f.reg.All() {
		expected[p.TrackerID] = p.PubKey
	}
	res, err := RunHandshakeListener(context.Background(), c, f.cfg.MyTrackerID, f.cfg.MyPriv, expected, f.cfg.HandshakeTimeout)
	if err != nil {
		f.dep.Metrics.InvalidFrames("handshake")
		_ = c.Close()
		return
	}
	f.attachPeer(res, c)
}

func (f *Federation) attachPeer(res HandshakeResult, c PeerConn) {
	dispatch := f.makeDispatcher(c, res.PeerTrackerID)
	pe := NewPeerForTest(c, dispatch)
	f.mu.Lock()
	f.peers[res.PeerTrackerID] = pe
	_ = f.reg.Update(PeerInfo{TrackerID: res.PeerTrackerID, PubKey: res.PeerPubKey, Addr: c.RemoteAddr(), State: PeerStateSteady, Conn: c})
	f.mu.Unlock()
	pe.Start(context.Background())
}

// makeDispatcher returns the recvLoop callback for one peer. The
// callback verifies the envelope sig, dedupes by message_id, dispatches
// by kind to apply or equiv, and counts metrics.
func (f *Federation) makeDispatcher(c PeerConn, peerID ids.TrackerID) func(*fed.Envelope) {
	return func(env *fed.Envelope) {
		f.dep.Metrics.FramesIn(env.Kind.String())
		if err := VerifyEnvelope(c.RemotePub(), env); err != nil {
			f.dep.Metrics.InvalidFrames("sig")
			return
		}
		mid := MessageID(env)
		if f.dedupe.Seen(mid) {
			f.dep.Metrics.InvalidFrames("dedupe_replay")
			return
		}
		f.dedupe.Mark(mid)
		switch env.Kind {
		case fed.Kind_KIND_ROOT_ATTESTATION:
			if err := f.apply.Apply(context.Background(), env); err != nil {
				if errors.Is(err, ErrEquivocation) {
					f.dep.Metrics.EquivocationsDetected()
				}
				f.dep.Metrics.RootAttestationsReceived(equivOutcome(err))
				return
			}
			f.dep.Metrics.RootAttestationsReceived("archived")
		case fed.Kind_KIND_EQUIVOCATION_EVIDENCE:
			f.equiv.OnIncomingEvidence(context.Background(), env, peerID)
		case fed.Kind_KIND_PING, fed.Kind_KIND_PONG:
			// keepalive — no action in this slice beyond the metric.
		default:
			f.dep.Metrics.InvalidFrames(fmt.Sprintf("kind_%d", int(env.Kind)))
		}
	}
}

func equivOutcome(err error) string {
	switch {
	case err == nil:
		return "archived"
	case errors.Is(err, ErrEquivocation):
		return "conflict"
	default:
		return "error"
	}
}

// registryPeerSet adapts *Registry into PeerSet by walking its active
// peers and returning the live *Peer behind each. Pointer receiver so
// the subsystem can fill f after Federation is constructed.
type registryPeerSet struct{ f *Federation }

func (r *registryPeerSet) ActivePeers() []SendOnlyPeer {
	if r == nil || r.f == nil {
		return nil
	}
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	out := make([]SendOnlyPeer, 0, len(r.f.peers))
	for id, pe := range r.f.peers {
		out = append(out, peerSendAdapter{id: id, peer: pe})
	}
	return out
}

type peerSendAdapter struct {
	id   ids.TrackerID
	peer *Peer
}

func (p peerSendAdapter) ID() ids.TrackerID { return p.id }
func (p peerSendAdapter) Send(ctx context.Context, frame []byte) error {
	return p.peer.Send(ctx, frame)
}

// ApplyForTest is a test-only shim: build a ROOT_ATTESTATION envelope
// from (peerID, peerPriv, hour, root, sig) and route it through the
// applier. Returns the applier's error verbatim. Bypasses the recv loop
// on purpose — full recv coverage lives in the real-transport slice.
//
// The peerPriv parameter signs the envelope; the test caller must keep
// it consistent with the peerID (sha256(pubkey) == peerID) for the
// applier's tracker_id-vs-sender_id check to pass.
func (f *Federation) ApplyForTest(peerID ids.TrackerID, peerPriv ed25519.PrivateKey, hour uint64, root, sig []byte) error {
	idBytes := peerID.Bytes()
	msg := &fed.RootAttestation{TrackerId: idBytes[:], Hour: hour, MerkleRoot: root, TrackerSig: sig}
	payload, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	env, err := SignEnvelope(peerPriv, idBytes[:], fed.Kind_KIND_ROOT_ATTESTATION, payload)
	if err != nil {
		return err
	}
	return f.apply.Apply(context.Background(), env)
}

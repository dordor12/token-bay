package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
)

// SendOnlyPeer is the slice of *Peer the gossip layer needs. Implementing
// this interface (rather than depending on *Peer directly) makes
// gossip_test.go independent of recv-loop wiring.
type SendOnlyPeer interface {
	ID() ids.TrackerID
	Send(ctx context.Context, frame []byte) error
}

// PeerSet is the slice of *Registry the gossip layer needs. The default
// implementation walks Registry.All() and filters non-steady peers; the
// interface enables a fake in tests.
type PeerSet interface {
	ActivePeers() []SendOnlyPeer
}

// Gossip wraps the forward-to-others orchestrator. Construct via
// NewGossip; tests use NewGossipForTest with a static list of peers.
//
// peers is a PeerSet — typically a *registryPeerSet whose pointer-typed
// receiver lets the subsystem fill in its *Federation back-reference
// AFTER NewGossip has captured the interface value.
//
// All fields are set at construction time and never mutated; WithObserver
// returns a shallow copy rather than mutating the receiver, so no mutex
// is needed.
type Gossip struct {
	priv    ed25519.PrivateKey
	myID    ids.TrackerID
	peers   PeerSet
	observe func([32]byte, fed.Kind)
}

func NewGossip(priv ed25519.PrivateKey, myID ids.TrackerID, peers PeerSet) *Gossip {
	return &Gossip{priv: priv, myID: myID, peers: peers}
}

func NewGossipForTest(peers []SendOnlyPeer, priv ed25519.PrivateKey, myID ids.TrackerID) *Gossip {
	return &Gossip{priv: priv, myID: myID, peers: staticPeerSet(peers)}
}

// WithObserver returns a shallow copy with a new observer set. Tests use
// this to assert message IDs without scraping peer state. The returned
// *Gossip shares the same PeerSet as the receiver.
func (g *Gossip) WithObserver(fn func([32]byte, fed.Kind)) *Gossip {
	cp := *g
	cp.observe = fn
	return &cp
}

// Forward sends payload (already-marshaled inner message) to every active
// peer except optional exclude. Returns nil even if some peers fail —
// gossip is best-effort. Errors are surfaced through metrics, not return
// values.
func (g *Gossip) Forward(ctx context.Context, kind fed.Kind, payload []byte, exclude *ids.TrackerID) error {
	// Bytes() returns by value (unaddressable for slicing), so assign first.
	idBytes := g.myID.Bytes()
	env, err := SignEnvelope(g.priv, idBytes[:], kind, payload)
	if err != nil {
		return err
	}
	frame, err := MarshalFrame(env)
	if err != nil {
		return err
	}
	mid := sha256.Sum256(payload)
	if g.observe != nil {
		g.observe(mid, kind)
	}
	for _, p := range g.peers.ActivePeers() {
		if exclude != nil && p.ID() == *exclude {
			continue
		}
		_ = p.Send(ctx, frame) // best-effort; per-peer error handling is the caller's metric problem
	}
	return nil
}

type staticPeerSet []SendOnlyPeer

func (s staticPeerSet) ActivePeers() []SendOnlyPeer { return []SendOnlyPeer(s) }

var _ = errors.New

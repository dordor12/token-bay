package federation

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// DepeerReason classifies why a peer was removed.
type DepeerReason int

const (
	ReasonOperator DepeerReason = iota
	ReasonEquivocation
	ReasonHandshakeFailed
	ReasonInvalidSignature
	ReasonDisconnected
)

func (r DepeerReason) String() string {
	switch r {
	case ReasonOperator:
		return "operator"
	case ReasonEquivocation:
		return "equivocation"
	case ReasonHandshakeFailed:
		return "handshake_failed"
	case ReasonInvalidSignature:
		return "invalid_signature"
	case ReasonDisconnected:
		return "disconnected"
	}
	return "unknown"
}

// PeerInfo is the operator-facing snapshot of one peer.
type PeerInfo struct {
	TrackerID ids.TrackerID
	PubKey    ed25519.PublicKey
	Addr      string
	Region    string
	State     PeerState
	Conn      PeerConn // nil unless State == PeerStateSteady
	Since     time.Time
}

// PeerState tracks the lifecycle of a peer connection.
type PeerState int

const (
	PeerStatePending PeerState = iota
	PeerStateDialing
	PeerStateHandshake
	PeerStateSteady
	PeerStateClosed
)

func (s PeerState) String() string {
	switch s {
	case PeerStatePending:
		return "pending"
	case PeerStateDialing:
		return "dialing"
	case PeerStateHandshake:
		return "handshake"
	case PeerStateSteady:
		return "steady"
	case PeerStateClosed:
		return "closed"
	}
	return "unknown"
}

// Registry holds the active peer set keyed by TrackerID.
type Registry struct {
	mu    sync.RWMutex
	peers map[ids.TrackerID]PeerInfo
}

// NewRegistry returns an empty, ready-to-use Registry.
func NewRegistry() *Registry {
	return &Registry{peers: make(map[ids.TrackerID]PeerInfo)}
}

// Add inserts a new peer. Returns an error if the TrackerID is already present
// or if PubKey is not the correct size.
func (r *Registry) Add(p PeerInfo) error {
	if len(p.PubKey) != ed25519.PublicKeySize {
		return errors.New("registry: PubKey must be 32 bytes")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.peers[p.TrackerID]; ok {
		return fmt.Errorf("registry: duplicate tracker_id %x", p.TrackerID.Bytes())
	}
	r.peers[p.TrackerID] = p
	return nil
}

// Update replaces the record for an existing peer; returns ErrPeerUnknown
// if the peer was never Added.
func (r *Registry) Update(p PeerInfo) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.peers[p.TrackerID]; !ok {
		return ErrPeerUnknown
	}
	r.peers[p.TrackerID] = p
	return nil
}

// Get returns the PeerInfo for id and whether it was found.
func (r *Registry) Get(id ids.TrackerID) (PeerInfo, bool) {
	r.mu.RLock()
	p, ok := r.peers[id]
	r.mu.RUnlock()
	return p, ok
}

// IsActive reports whether a peer is known and in the steady state.
func (r *Registry) IsActive(id ids.TrackerID) bool {
	p, ok := r.Get(id)
	return ok && p.State == PeerStateSteady
}

// All returns a copy slice of all known peers.
func (r *Registry) All() []PeerInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]PeerInfo, 0, len(r.peers))
	for _, p := range r.peers {
		out = append(out, p)
	}
	return out
}

// Depeer removes a peer from the active map, closing its connection if any.
// Returns ErrPeerUnknown if the peer is not present. Idempotent in the sense
// that a second call returns ErrPeerUnknown rather than panicking.
func (r *Registry) Depeer(id ids.TrackerID, reason DepeerReason) error {
	r.mu.Lock()
	p, ok := r.peers[id]
	if !ok {
		r.mu.Unlock()
		return ErrPeerUnknown
	}
	delete(r.peers, id)
	r.mu.Unlock()
	if p.Conn != nil {
		_ = p.Conn.Close()
	}
	_ = reason // metric / log emission lives in the caller
	return nil
}

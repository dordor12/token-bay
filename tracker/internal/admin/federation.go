package admin

import (
	"errors"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// Errors returned by FederationView implementations. The cmd-side adapter
// translates federation.ErrPeerExists / federation.ErrPeerUnknown to these
// so admin handlers can map outcomes to HTTP codes without importing the
// federation package.
var (
	// ErrPeerExists is returned by AddPeer when the TrackerID is already
	// known to the subsystem. Mapped to 409 Conflict.
	ErrPeerExists = errors.New("admin: peer already registered")
	// ErrPeerUnknown is returned by Depeer when the TrackerID is not in
	// the active set. Mapped to 404 Not Found.
	ErrPeerUnknown = errors.New("admin: peer unknown")
)

// PeerSummary is the operator-facing snapshot of one federation peer,
// flattened from federation.PeerInfo plus the computed health score.
type PeerSummary struct {
	TrackerID   ids.TrackerID
	PubKey      []byte
	Addr        string
	Region      string
	State       string
	Since       time.Time
	HealthScore float64
}

// PeerAddRequest is the operator's request to add an allowlisted peer.
// All fields are required; PubKey must be 32 bytes (Ed25519 public key).
type PeerAddRequest struct {
	TrackerID ids.TrackerID
	PubKey    []byte
	Addr      string
	Region    string
}

// FederationView is the admin-facing surface of the federation subsystem.
// When the admin server is constructed without a FederationView, /peers
// reports the subsystem as disabled and the mutation endpoints return
// 501 Not Implemented.
type FederationView interface {
	// Peers returns a value snapshot of all known peers with their
	// current health score. Safe to call concurrently.
	Peers() []PeerSummary

	// AddPeer registers an operator-allowlisted peer at runtime,
	// spawning the redial goroutine. Returns ErrPeerExists if the
	// TrackerID is already registered.
	AddPeer(req PeerAddRequest) error

	// Depeer removes a peer from the active set, closing its connection
	// if any. Returns ErrPeerUnknown if the peer is not registered.
	Depeer(trackerID ids.TrackerID) error

	// ListenAddr returns the bound peering port, or "" when the
	// transport is not network-bound.
	ListenAddr() string
}

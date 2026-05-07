package session

import (
	"crypto/ed25519"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// State is the in-flight request lifecycle state. Transitions are CAS-guarded
// by Inflight.Transition. See tracker-broker-design §4.2.
type State uint8

const (
	StateUnspecified State = iota
	StateSelecting
	StateAssigned
	StateServing
	StateCompleted
	StateFailed
)

// String returns a stable label suitable for metrics and admin output.
// Locked to a small enumeration so metric label cardinality stays bounded.
func (s State) String() string {
	switch s {
	case StateSelecting:
		return "selecting"
	case StateAssigned:
		return "assigned"
	case StateServing:
		return "serving"
	case StateCompleted:
		return "completed"
	case StateFailed:
		return "failed"
	default:
		return "unspecified"
	}
}

// IsAllowedTransition reports whether (from → to) is a permitted state
// transition per the broker spec §5 state diagram.
func IsAllowedTransition(from, to State) bool {
	switch from {
	case StateSelecting:
		return to == StateAssigned || to == StateFailed
	case StateAssigned:
		return to == StateServing || to == StateFailed
	case StateServing:
		return to == StateCompleted || to == StateFailed
	}
	return false
}

// Request is one in-flight broker request, tracked from selection through
// settlement. Fields populate progressively as the request advances.
type Request struct {
	RequestID       [16]byte
	ConsumerID      ids.IdentityID
	EnvelopeBody    *tbproto.EnvelopeBody
	EnvelopeHash    [32]byte
	MaxCostReserved uint64
	AssignedSeeder  ids.IdentityID
	SeederPubkey    ed25519.PublicKey
	OfferAttempts   []ids.IdentityID
	StartedAt       time.Time
	State           State

	// Settlement coordination — populated on usage_report:
	PreimageHash [32]byte
	SettleSig    chan []byte
	TerminatedAt time.Time
}

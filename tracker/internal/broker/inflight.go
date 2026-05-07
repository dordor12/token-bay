package broker

import (
	"crypto/ed25519"
	"sync"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// State is the in-flight request lifecycle state. Transitions are CAS-guarded
// by Inflight.Transition. See broker-design §4.2.
type State uint8

const (
	StateUnspecified State = iota
	StateSelecting
	StateAssigned
	StateServing
	StateCompleted
	StateFailed
)

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

// Inflight is the in-memory request registry. Safe for concurrent use.
type Inflight struct {
	mu     sync.RWMutex
	byID   map[[16]byte]*Request
	byHash map[[32]byte]*Request
}

func NewInflight() *Inflight {
	return &Inflight{
		byID:   make(map[[16]byte]*Request),
		byHash: make(map[[32]byte]*Request),
	}
}

func (f *Inflight) Insert(r *Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[r.RequestID] = r
}

func (f *Inflight) Get(id [16]byte) (*Request, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	r, ok := f.byID[id]
	return r, ok
}

// Transition is a CAS state-change. Returns ErrUnknownRequest if the request
// is not present, ErrIllegalTransition if the current state is not `from`.
// Concurrent Transition calls win exactly once for any given (from, to);
// losers see ErrIllegalTransition.
func (f *Inflight) Transition(id [16]byte, from, to State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.byID[id]
	if !ok {
		return ErrUnknownRequest
	}
	if r.State != from {
		return ErrIllegalTransition
	}
	r.State = to
	if to == StateCompleted || to == StateFailed {
		r.TerminatedAt = time.Now()
	}
	return nil
}

func (f *Inflight) MarkSeeder(id [16]byte, seeder ids.IdentityID, pub ed25519.PublicKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.byID[id]
	if !ok {
		return ErrUnknownRequest
	}
	r.AssignedSeeder = seeder
	r.SeederPubkey = pub
	return nil
}

// IndexByHash records the preimage hash of the in-flight request so a later
// HandleSettle call can find it by hash. The mapping is dropped when the
// request is swept after entering a terminal state.
func (f *Inflight) IndexByHash(reqID [16]byte, hash [32]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.byID[reqID]
	if !ok {
		return ErrUnknownRequest
	}
	r.PreimageHash = hash
	f.byHash[hash] = r
	return nil
}

func (f *Inflight) LookupByHash(hash [32]byte) (*Request, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	r, ok := f.byHash[hash]
	return r, ok
}

// Sweep removes terminal-state entries whose TerminatedAt is older than
// terminalTTL. Returns the dropped requests for diagnostics. Drops the
// byHash mapping for swept entries.
func (f *Inflight) Sweep(now time.Time, terminalTTL time.Duration) []*Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var swept []*Request
	cutoff := now.Add(-terminalTTL)
	for id, r := range f.byID {
		if (r.State == StateCompleted || r.State == StateFailed) && !r.TerminatedAt.After(cutoff) {
			swept = append(swept, r)
			delete(f.byID, id)
			if r.PreimageHash != ([32]byte{}) {
				delete(f.byHash, r.PreimageHash)
			}
		}
	}
	return swept
}

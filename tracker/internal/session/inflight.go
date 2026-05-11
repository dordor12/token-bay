package session

import (
	"crypto/ed25519"
	"sync"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

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

// LookupAssignment returns the consumer and assigned seeder for an in-flight
// request keyed by request_id. ok=false if the request is unknown or selection
// has not yet produced a seeder assignment. Safe to call concurrently with
// MarkSeeder / Transition.
func (f *Inflight) LookupAssignment(reqID [16]byte) (consumer, seeder ids.IdentityID, ok bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	r, present := f.byID[reqID]
	if !present {
		return ids.IdentityID{}, ids.IdentityID{}, false
	}
	if r.AssignedSeeder == (ids.IdentityID{}) {
		return ids.IdentityID{}, ids.IdentityID{}, false
	}
	return r.ConsumerID, r.AssignedSeeder, true
}

// EnsureSettleSig idempotently initializes the request's settlement-sig
// dispatch channel. Safe to call from HandleUsageReport before IndexByHash;
// the lock here happens-before any HandleSettle's LookupByHash, so a later
// HandleSettle observes a non-nil channel without further synchronization.
func (f *Inflight) EnsureSettleSig(reqID [16]byte) (chan []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.byID[reqID]
	if !ok {
		return nil, ErrUnknownRequest
	}
	if r.SettleSig == nil {
		r.SettleSig = make(chan []byte, 1)
	}
	return r.SettleSig, nil
}

// InflightSummary is a snapshot row for admin listing. Carries enough to
// render the admin /broker/inflight list without leaking the underlying
// pointer.
type InflightSummary struct {
	RequestID      [16]byte
	ConsumerID     ids.IdentityID
	State          State
	StartedAt      time.Time
	AssignedSeeder ids.IdentityID
	PreimageHash   [32]byte
}

// Snapshot returns a stable copy of every in-flight summary at call time.
// Used by admin handlers; not on the hot path.
func (f *Inflight) Snapshot() []InflightSummary {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]InflightSummary, 0, len(f.byID))
	for _, r := range f.byID {
		out = append(out, InflightSummary{
			RequestID:      r.RequestID,
			ConsumerID:     r.ConsumerID,
			State:          r.State,
			StartedAt:      r.StartedAt,
			AssignedSeeder: r.AssignedSeeder,
			PreimageHash:   r.PreimageHash,
		})
	}
	return out
}

// ForceFail unconditionally sets the request to StateFailed regardless of
// prior state. Returns the prior state and ok=true on success; ok=false if
// the request doesn't exist. Operator-only — bypasses the IsAllowedTransition
// check that Transition enforces.
func (f *Inflight) ForceFail(reqID [16]byte, now time.Time) (State, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.byID[reqID]
	if !ok {
		return StateUnspecified, false
	}
	prev := r.State
	r.State = StateFailed
	if r.TerminatedAt.IsZero() {
		r.TerminatedAt = now
	}
	return prev, true
}

// SweepTerminal removes terminal-state entries whose TerminatedAt is older
// than terminalTTL. Returns the dropped requests for diagnostics. Drops the
// byHash mapping for swept entries.
func (f *Inflight) SweepTerminal(now time.Time, terminalTTL time.Duration) []*Request {
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

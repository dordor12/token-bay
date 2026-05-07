package session

import (
	"sync"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// Reservation is one in-memory reservation slot. The TTL reaper sweeps slots
// whose ExpiresAt has passed; the broker's selection / settlement paths
// release them explicitly via Release(reqID).
type Reservation struct {
	ReqID      [16]byte
	ConsumerID ids.IdentityID
	Amount     uint64
	ExpiresAt  time.Time
}

// Reservations is the in-memory credit reservation ledger described in
// tracker-broker-design §4.1. Safe for concurrent use.
type Reservations struct {
	mu    sync.Mutex
	byID  map[ids.IdentityID]uint64
	byReq map[[16]byte]Reservation
}

func NewReservations() *Reservations {
	return &Reservations{
		byID:  make(map[ids.IdentityID]uint64),
		byReq: make(map[[16]byte]Reservation),
	}
}

// Reserve debits `amount` against `snapshotCredits − Reserved(consumer)`.
// Returns ErrInsufficientCredits when the consumer's working balance after
// existing reservations cannot cover `amount`. Returns ErrDuplicateReservation
// if `reqID` is already present.
func (r *Reservations) Reserve(reqID [16]byte, consumer ids.IdentityID, amount, snapshotCredits uint64, expiresAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byReq[reqID]; exists {
		return ErrDuplicateReservation
	}
	if r.byID[consumer]+amount > snapshotCredits {
		return ErrInsufficientCredits
	}
	r.byID[consumer] += amount
	r.byReq[reqID] = Reservation{ReqID: reqID, ConsumerID: consumer, Amount: amount, ExpiresAt: expiresAt}
	return nil
}

// Release frees the reservation slot. Idempotent: a repeat call returns
// ok=false. The first successful release returns the consumer + amount so
// the caller can record metrics.
func (r *Reservations) Release(reqID [16]byte) (ids.IdentityID, uint64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	slot, ok := r.byReq[reqID]
	if !ok {
		return ids.IdentityID{}, 0, false
	}
	delete(r.byReq, reqID)
	r.byID[slot.ConsumerID] -= slot.Amount
	if r.byID[slot.ConsumerID] == 0 {
		delete(r.byID, slot.ConsumerID)
	}
	return slot.ConsumerID, slot.Amount, true
}

// Reserved returns the consumer's currently reserved total.
func (r *Reservations) Reserved(consumer ids.IdentityID) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byID[consumer]
}

// SweepExpired removes any slot whose ExpiresAt is at or before `now`. Returns
// the dropped slots so the caller can metric/log.
func (r *Reservations) SweepExpired(now time.Time) []Reservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	var expired []Reservation
	for id, slot := range r.byReq {
		if !slot.ExpiresAt.After(now) {
			expired = append(expired, slot)
			delete(r.byReq, id)
			r.byID[slot.ConsumerID] -= slot.Amount
			if r.byID[slot.ConsumerID] == 0 {
				delete(r.byID, slot.ConsumerID)
			}
		}
	}
	return expired
}

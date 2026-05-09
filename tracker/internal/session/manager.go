package session

import (
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// Manager bundles the two stores so callers can construct lifecycle state
// with a single helper. Both fields are exported; callers may interact via
// mgr.Inflight.X / mgr.Reservations.Y or via Manager-level helpers like
// SweepExpiredAndFail.
type Manager struct {
	Inflight     *Inflight
	Reservations *Reservations
}

// New constructs a Manager with fresh empty stores.
func New() *Manager {
	return &Manager{
		Inflight:     NewInflight(),
		Reservations: NewReservations(),
	}
}

// Expiry is one record returned by SweepExpiredAndFail. The caller (broker
// reaper) uses PriorState + AssignedSeeder to drive registry-side
// compensations such as Registry.DecLoad.
type Expiry struct {
	ReqID          [16]byte
	ConsumerID     ids.IdentityID
	Amount         uint64
	PriorState     State
	AssignedSeeder ids.IdentityID // zero unless PriorState == StateAssigned
}

// SweepExpiredAndFail does, atomically per slot:
//  1. drop the reservation slot;
//  2. read the matching Request's current state;
//  3. if PriorState ∈ {StateSelecting, StateAssigned}, transition to StateFailed.
//
// Returns one Expiry per dropped slot. SERVING / COMPLETED / FAILED entries
// observed during sweep are reported but NOT transitioned — that's an
// operator-attention path (broker spec §7.16) where the reservation needs
// manual release rather than an automatic Failed transition.
func (m *Manager) SweepExpiredAndFail(now time.Time) []Expiry {
	expired := m.Reservations.SweepExpired(now)
	if len(expired) == 0 {
		return nil
	}
	out := make([]Expiry, 0, len(expired))
	for _, slot := range expired {
		e := Expiry{
			ReqID:      slot.ReqID,
			ConsumerID: slot.ConsumerID,
			Amount:     slot.Amount,
		}
		if req, ok := m.Inflight.Get(slot.ReqID); ok {
			e.PriorState = req.State
			if req.State == StateAssigned {
				e.AssignedSeeder = req.AssignedSeeder
			}
			switch req.State {
			case StateSelecting:
				_ = m.Inflight.Transition(slot.ReqID, StateSelecting, StateFailed)
			case StateAssigned:
				_ = m.Inflight.Transition(slot.ReqID, StateAssigned, StateFailed)
			}
		}
		out = append(out, e)
	}
	return out
}

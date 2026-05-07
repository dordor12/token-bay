package broker

import (
	"time"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/session"
)

// startReaper launches the reservation TTL reaper goroutine. It fires every
// 30 seconds and calls runReap to sweep expired reservation slots and
// transition their inflight requests to StateFailed. See broker-design §5.5.
func (s *Settlement) startReaper() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case now := <-ticker.C:
				s.runReap(now)
			}
		}
	}()
}

// runReap sweeps all expired reservation slots and transitions their
// associated inflight requests to StateFailed. The state CAS lives in
// session.Manager.SweepExpiredAndFail; broker drives the registry-side
// compensation here (DecLoad on AssignedSeeder for Assigned-state expirations).
func (s *Settlement) runReap(now time.Time) {
	expired := s.mgr.SweepExpiredAndFail(now)
	for _, e := range expired {
		if e.PriorState == session.StateAssigned && e.AssignedSeeder != (ids.IdentityID{}) {
			_, _ = s.deps.Registry.DecLoad(e.AssignedSeeder)
		}
	}
}

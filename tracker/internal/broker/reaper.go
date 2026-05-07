package broker

import (
	"time"

	"github.com/token-bay/token-bay/shared/ids"
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
// associated inflight requests to StateFailed. For ASSIGNED requests,
// it also decrements the assigned seeder's load counter.
func (s *Settlement) runReap(now time.Time) {
	expired := s.resv.Sweep(now)
	for _, slot := range expired {
		req, ok := s.inflt.Get(slot.ReqID)
		if !ok {
			continue
		}
		switch req.State {
		case StateSelecting:
			_ = s.inflt.Transition(slot.ReqID, StateSelecting, StateFailed)
		case StateAssigned:
			if req.AssignedSeeder != (ids.IdentityID{}) {
				_, _ = s.deps.Registry.DecLoad(req.AssignedSeeder)
			}
			_ = s.inflt.Transition(slot.ReqID, StateAssigned, StateFailed)
		}
	}
}

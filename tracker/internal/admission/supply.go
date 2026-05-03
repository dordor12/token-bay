package admission

import "time"

// SupplySnapshot is the published view of regional supply pressure that
// Decide reads on the hot path. Aggregator (Task 7) writes via
// publishSupply; Decide reads via Supply. Reads never block — the field
// is an atomic.Pointer[SupplySnapshot] on the Subsystem.
//
// Spec: admission-design §4.2.
type SupplySnapshot struct {
	ComputedAt          time.Time          // zero value indicates "never computed yet"
	TotalHeadroom       float64            // sum of weighted seeder contributions
	PerModel            map[string]float64 // per-model breakdown (admission-design §5.3 step 2)
	ContributingSeeders uint32             // count of seeders with weight > 0
	SilentSeeders       uint32             // count of seeders dropped by freshness decay
	Pressure            float64            // demand_rate_ewma / TotalHeadroom; 0 if TotalHeadroom == 0
	RecentRejectRate    float64            // OVER_CAPACITY rejects per minute, EWMA
}

// emptySupplySnapshot is returned by Supply() before the aggregator has
// run for the first time. Decide treats this as "no supply data yet" and
// admits unconditionally — at boot the network has no rate limit until
// the 5s aggregator first publishes.
var emptySupplySnapshot = &SupplySnapshot{}

// Supply returns the most-recently-published SupplySnapshot. Never nil.
// Pre-publish callers receive emptySupplySnapshot (ComputedAt zero).
func (s *Subsystem) Supply() *SupplySnapshot {
	if v := s.supply.Load(); v != nil {
		return v
	}
	return emptySupplySnapshot
}

// publishSupply atomically swaps the current snapshot. Aggregator
// (Task 7) is the only caller in production; tests use it directly.
func (s *Subsystem) publishSupply(snap *SupplySnapshot) {
	s.supply.Store(snap)
}

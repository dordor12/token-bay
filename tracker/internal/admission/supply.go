package admission

import (
	"math"
	"sync"
	"time"

	"github.com/token-bay/token-bay/tracker/internal/registry"
)

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
	if s.metrics != nil && snap != nil {
		s.metrics.Pressure.Set(snap.Pressure)
		s.metrics.SupplyTotal.Set(snap.TotalHeadroom)
		s.metrics.SeedersContrib.Set(float64(snap.ContributingSeeders))
	}
}

// aggregator state. Demand EWMA tracks broker_request arrival rate;
// every recordDemandTick increments a running counter that decays each
// time the aggregator runs. Half-life ≈ 5s (admission-design §5.1 step 3).
type demandTracker struct {
	mu       sync.Mutex
	rateEWMA float64 // events per second
	lastTick time.Time
}

func (d *demandTracker) record(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rateEWMA += 1.0 // raw count; decayed when read
	d.lastTick = now
}

// readAndDecay returns the current rate and decays it by exp(-Δt/halfLife).
// Half-life 5s.
func (d *demandTracker) readAndDecay(now time.Time) float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastTick.IsZero() {
		return 0
	}
	dt := now.Sub(d.lastTick).Seconds()
	if dt < 0 {
		dt = 0
	}
	d.rateEWMA *= math.Exp(-dt / 5.0)
	d.lastTick = now
	return d.rateEWMA
}

// recordDemandTick is called from Decide every time a broker_request
// arrives. Cheap; aggregator drains the EWMA.
func (s *Subsystem) recordDemandTick(now time.Time) {
	s.demand.record(now)
}

// runAggregatorOnce executes one aggregation pass. Exposed for tests
// (the production path drives it via aggregatorTick).
func (s *Subsystem) runAggregatorOnce(now time.Time) {
	seeders := s.reg.Snapshot()
	var (
		total        float64
		perModel     = make(map[string]float64)
		contributing uint32
		silent       uint32
	)
	for _, sr := range seeders {
		w := s.seederWeight(sr, now)
		if w <= 0 {
			silent++
			continue
		}
		contribution := sr.HeadroomEstimate * w
		total += contribution
		contributing++
		for _, model := range sr.Capabilities.Models {
			perModel[model] += contribution
		}
	}

	demand := s.demand.readAndDecay(now)
	pressure := 0.0
	if total > 0 {
		pressure = demand / total
	}
	s.publishSupply(&SupplySnapshot{
		ComputedAt:          now,
		TotalHeadroom:       total,
		PerModel:            perModel,
		ContributingSeeders: contributing,
		SilentSeeders:       silent,
		Pressure:            pressure,
	})
}

// seederWeight combines freshness decay and heartbeat-reliability
// weighting per admission-design §5.3. Result is in [0, 1].
//
// freshness_decay: 1.0 if last_heartbeat < 60s ago; linear decay to 0
// at HeartbeatFreshnessDecayMaxS; 0 thereafter.
// heartbeat_reliability: from SeederHeartbeatState; v1 returns 1.0 when
// no state exists yet (§5.5: the heartbeat-reliability signal becomes
// meaningful only after the rolling window has data).
func (s *Subsystem) seederWeight(sr registry.SeederRecord, now time.Time) float64 {
	age := now.Sub(sr.LastHeartbeat).Seconds()
	if age < 0 {
		age = 0
	}
	maxAge := float64(s.cfg.HeartbeatFreshnessDecayMaxS)
	const freshThresholdS = 60.0
	var freshness float64
	switch {
	case age <= freshThresholdS:
		freshness = 1.0
	case age >= maxAge:
		freshness = 0
	default:
		freshness = (maxAge - age) / (maxAge - freshThresholdS)
	}
	return freshness * sr.ReputationScore
}

// runAggregatorLoop is the goroutine body. Consumes tick events from
// aggregatorTick and runs runAggregatorOnce per tick. Stops on s.stop.
func (s *Subsystem) runAggregatorLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.stop:
			return
		case now := <-s.aggregatorTick:
			s.runAggregatorOnce(now)
		}
	}
}

// startAggregator spawns the aggregator goroutine and the 5s ticker
// that feeds it. Tests can pre-fill aggregatorTick to drive deterministic
// runs without involving real time.
func (s *Subsystem) startAggregator() {
	s.aggregatorTick = make(chan time.Time, 1)
	s.wg.Add(1)
	go s.runAggregatorLoop()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case now := <-t.C:
				select {
				case s.aggregatorTick <- now:
				default:
					// drop tick if loop is busy — next tick will catch up
				}
			}
		}
	}()
}

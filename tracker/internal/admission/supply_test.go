package admission

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSupply_PrePublish_ReturnsZeroSnapshotMarker(t *testing.T) {
	s, _ := openTempSubsystem(t)
	snap := s.Supply()
	require.NotNil(t, snap, "Supply must never return nil — callers always get a snapshot")
	assert.True(t, snap.ComputedAt.IsZero(), "pre-publish snapshot has zero ComputedAt as 'never computed' marker")
	assert.Equal(t, 0.0, snap.TotalHeadroom)
}

func TestSupply_PublishThenLoad(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	want := &SupplySnapshot{
		ComputedAt:    now,
		TotalHeadroom: 5.0,
		Pressure:      0.6,
	}
	s.publishSupply(want)

	got := s.Supply()
	assert.Equal(t, want, got)
}

func TestSupply_ConcurrentLoadStore_RaceClean(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: float64(i)})
				}
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = s.Supply()
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	// Race detector catches violations; pass = no race reported.
}

func TestAggregator_TickProducesSnapshot(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	clk := newFixedClock(now)
	s, _ := openTempSubsystem(t, WithClock(clk.Now))

	// Seed the registry with two seeders, both reachable.
	registerSeeder(t, s.reg, makeID(0xAA), 0.7 /*headroom*/, now /*last hb*/)
	registerSeeder(t, s.reg, makeID(0xBB), 0.5, now)

	// Drive one aggregator tick.
	s.runAggregatorOnce(now)

	snap := s.Supply()
	assert.Equal(t, now, snap.ComputedAt)
	assert.Greater(t, snap.TotalHeadroom, 0.0)
	assert.Equal(t, uint32(2), snap.ContributingSeeders)
	assert.Equal(t, uint32(0), snap.SilentSeeders)
}

func TestAggregator_FreshnessDecay_DropsStaleSeeders(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	clk := newFixedClock(now)
	s, _ := openTempSubsystem(t, WithClock(clk.Now))

	// One fresh seeder, one stale (heartbeat older than freshness window).
	registerSeeder(t, s.reg, makeID(0xAA), 0.8, now)
	registerSeeder(t, s.reg, makeID(0xBB), 0.8, now.Add(-time.Duration(s.cfg.HeartbeatFreshnessDecayMaxS+10)*time.Second))

	s.runAggregatorOnce(now)

	snap := s.Supply()
	assert.Equal(t, uint32(1), snap.ContributingSeeders)
	assert.Equal(t, uint32(1), snap.SilentSeeders)
}

func TestAggregator_PressureZeroWhenNoSupply(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	clk := newFixedClock(now)
	s, _ := openTempSubsystem(t, WithClock(clk.Now))

	s.runAggregatorOnce(now)
	snap := s.Supply()
	assert.Equal(t, 0.0, snap.TotalHeadroom)
	assert.Equal(t, 0.0, snap.Pressure, "pressure must be 0 (not NaN/Inf) when supply is zero")
}

func TestAggregator_DemandEWMA_AffectsPressure(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	clk := newFixedClock(now)
	s, _ := openTempSubsystem(t, WithClock(clk.Now))
	registerSeeder(t, s.reg, makeID(0xAA), 1.0, now)

	// Drive demand: 5 ticks back-to-back.
	for i := 0; i < 5; i++ {
		s.recordDemandTick(now)
	}
	s.runAggregatorOnce(now)
	snap1 := s.Supply()
	assert.Greater(t, snap1.Pressure, 0.0)

	// More demand → higher pressure.
	for i := 0; i < 50; i++ {
		s.recordDemandTick(now)
	}
	s.runAggregatorOnce(now)
	snap2 := s.Supply()
	assert.Greater(t, snap2.Pressure, snap1.Pressure)
}

func TestAggregator_BackgroundLoop_RunsOnTicker(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	clk := newFixedClock(now)
	s, _ := openTempSubsystem(t, WithClock(clk.Now))
	registerSeeder(t, s.reg, makeID(0xAA), 0.6, now)

	// Manually fire the aggregator tick channel.
	s.aggregatorTick <- now
	// Wait briefly for the goroutine to process — bounded loop with timeout.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.Supply().ComputedAt.Equal(now) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	assert.Equal(t, now, s.Supply().ComputedAt, "aggregator goroutine consumed the tick")
}

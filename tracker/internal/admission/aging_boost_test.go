package admission

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgingBoost_LowCreditWaitingOutranksFreshHigherCredit(t *testing.T) {
	t0 := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	clk := newFixedClock(t0)
	s, _ := openTempSubsystem(t, WithClock(clk.Now))
	// Force QUEUE: pressure between admit and reject thresholds.
	s.publishSupply(&SupplySnapshot{ComputedAt: t0, TotalHeadroom: 10, Pressure: 1.0})

	// Consumer A: low credit (0.3), seeded with thin local history.
	idA := makeID(0xAA)
	stA := consumerShardFor(s.consumerShards, idA).getOrInit(idA, t0.AddDate(0, 0, -3))
	stA.SettlementBuckets[0] = DayBucket{Total: 10, A: 3, DayStamp: stripToDay(t0)} // reliability 0.3
	stA.LastBalanceSeen = 100

	// Consumer B: higher credit (0.5).
	idB := makeID(0xBB)
	stB := consumerShardFor(s.consumerShards, idB).getOrInit(idB, t0.AddDate(0, 0, -10))
	stB.SettlementBuckets[0] = DayBucket{Total: 10, A: 5, DayStamp: stripToDay(t0)} // reliability 0.5
	stB.LastBalanceSeen = 100

	// A enqueues at t0.
	resA := s.Decide(idA, nil, t0)
	require.Equal(t, OutcomeQueue, resA.Outcome)

	// 9 minutes later, B enqueues.
	t1 := t0.Add(9 * time.Minute)
	clk.Advance(9 * time.Minute)
	resB := s.Decide(idB, nil, t1)
	require.Equal(t, OutcomeQueue, resB.Outcome)

	// Advance to t=10min and drain the queue's top.
	t2 := t0.Add(10 * time.Minute)
	clk.Advance(1 * time.Minute)

	s.queueMu.Lock()
	s.queue.AdvanceTime(t2)
	s.queue.Reheapify()
	first, ok := s.queue.Peek()
	s.queueMu.Unlock()

	require.True(t, ok)
	assert.Equal(t, idA, first.ConsumerID,
		"aged-A (0.3 credit, 10-min wait) outranks fresh-B (0.5 credit, 1-min wait) — admission-design §10 #6")

	// Effective priorities sanity:
	// A: 0.3 + 0.05 * 10 = 0.8
	// B: 0.5 + 0.05 * 1  = 0.55
}

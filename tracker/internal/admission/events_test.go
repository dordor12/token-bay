package admission

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOnLedgerEvent_SettlementClean_BumpsTotalAndA(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	consumer := makeID(0xC1)
	seeder := makeID(0x5E)
	s.OnLedgerEvent(LedgerEvent{
		Kind:        LedgerEventSettlement,
		ConsumerID:  consumer,
		SeederID:    seeder,
		CostCredits: 100,
		Flags:       0,
		Timestamp:   now,
	})

	cs, ok := consumerShardFor(s.consumerShards, consumer).get(consumer)
	require.True(t, ok)
	idx := dayBucketIndex(now, rollingWindowDays)
	assert.Equal(t, uint32(1), cs.SettlementBuckets[idx].Total)
	assert.Equal(t, uint32(1), cs.SettlementBuckets[idx].A, "clean settlement bumps A")
	assert.Equal(t, uint32(100), cs.FlowBuckets[idx].B, "consumer flow.Spent += cost")
	assert.Equal(t, int64(-100), cs.LastBalanceSeen)

	ss, ok := consumerShardFor(s.consumerShards, seeder).get(seeder)
	require.True(t, ok)
	assert.Equal(t, uint32(100), ss.FlowBuckets[idx].A, "seeder flow.Earned += cost")
	assert.Equal(t, int64(100), ss.LastBalanceSeen)
}

func TestOnLedgerEvent_SettlementWithFlag_BumpsTotalNotA(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	s.OnLedgerEvent(LedgerEvent{
		Kind:        LedgerEventSettlement,
		ConsumerID:  makeID(0xC1),
		SeederID:    makeID(0x5E),
		CostCredits: 100,
		Flags:       1,
		Timestamp:   now,
	})

	cs, _ := consumerShardFor(s.consumerShards, makeID(0xC1)).get(makeID(0xC1))
	idx := dayBucketIndex(now, rollingWindowDays)
	assert.Equal(t, uint32(1), cs.SettlementBuckets[idx].Total)
	assert.Equal(t, uint32(0), cs.SettlementBuckets[idx].A, "non-clean settlement does not bump A")
}

func TestOnLedgerEvent_StarterGrant_InitializesState(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	id := makeID(0x42)

	s.OnLedgerEvent(LedgerEvent{
		Kind:        LedgerEventStarterGrant,
		ConsumerID:  id,
		CostCredits: 1000,
		Timestamp:   now,
	})

	cs, ok := consumerShardFor(s.consumerShards, id).get(id)
	require.True(t, ok)
	assert.True(t, cs.FirstSeenAt.Equal(now))
	assert.Equal(t, int64(1000), cs.LastBalanceSeen)
}

func TestOnLedgerEvent_TransferIn_OutMirrorBalance(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	id := makeID(0x11)

	s.OnLedgerEvent(LedgerEvent{
		Kind: LedgerEventTransferIn, ConsumerID: id, CostCredits: 500, Timestamp: now,
	})
	cs, _ := consumerShardFor(s.consumerShards, id).get(id)
	assert.Equal(t, int64(500), cs.LastBalanceSeen)

	s.OnLedgerEvent(LedgerEvent{
		Kind: LedgerEventTransferOut, ConsumerID: id, CostCredits: 200, Timestamp: now,
	})
	cs2, _ := consumerShardFor(s.consumerShards, id).get(id)
	assert.Equal(t, int64(300), cs2.LastBalanceSeen)

	idx := dayBucketIndex(now, rollingWindowDays)
	assert.Equal(t, uint32(0), cs2.SettlementBuckets[idx].Total)
	assert.Equal(t, uint32(0), cs2.FlowBuckets[idx].A)
}

func TestOnLedgerEvent_DisputeFiledThenUpheld(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	id := makeID(0x11)

	s.OnLedgerEvent(LedgerEvent{Kind: LedgerEventDisputeFiled, ConsumerID: id, Timestamp: now})
	cs, _ := consumerShardFor(s.consumerShards, id).get(id)
	idx := dayBucketIndex(now, rollingWindowDays)
	assert.Equal(t, uint32(1), cs.DisputeBuckets[idx].A)
	assert.Equal(t, uint32(0), cs.DisputeBuckets[idx].B)

	s.OnLedgerEvent(LedgerEvent{Kind: LedgerEventDisputeResolved, ConsumerID: id, DisputeUpheld: true, Timestamp: now})
	cs2, _ := consumerShardFor(s.consumerShards, id).get(id)
	assert.Equal(t, uint32(1), cs2.DisputeBuckets[idx].B)

	s.OnLedgerEvent(LedgerEvent{Kind: LedgerEventDisputeResolved, ConsumerID: id, DisputeUpheld: false, Timestamp: now})
	cs3, _ := consumerShardFor(s.consumerShards, id).get(id)
	assert.Equal(t, uint32(1), cs3.DisputeBuckets[idx].B, "rejected dispute does not bump Upheld")
}

func TestOnLedgerEvent_StaleBucket_Rotates(t *testing.T) {
	s, _ := openTempSubsystem(t)
	id := makeID(0x11)

	t1 := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s.OnLedgerEvent(LedgerEvent{
		Kind: LedgerEventSettlement, ConsumerID: id, SeederID: makeID(0x5E),
		CostCredits: 100, Flags: 0, Timestamp: t1,
	})

	t2 := t1.AddDate(0, 0, 35)
	s.OnLedgerEvent(LedgerEvent{
		Kind: LedgerEventSettlement, ConsumerID: id, SeederID: makeID(0x5E),
		CostCredits: 50, Flags: 0, Timestamp: t2,
	})

	cs, _ := consumerShardFor(s.consumerShards, id).get(id)
	idx := dayBucketIndex(t2, rollingWindowDays)
	assert.Equal(t, uint32(1), cs.SettlementBuckets[idx].Total, "stale bucket must rotate to 0 before incrementing")
	assert.Equal(t, uint32(1), cs.SettlementBuckets[idx].A)
}

func TestOnLedgerEvent_UnspecifiedKind_NoOp(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	s.OnLedgerEvent(LedgerEvent{Kind: LedgerEventUnspecified, ConsumerID: makeID(0x11), Timestamp: now})
	_, ok := consumerShardFor(s.consumerShards, makeID(0x11)).get(makeID(0x11))
	assert.False(t, ok, "unspecified kind must not initialize state")
}

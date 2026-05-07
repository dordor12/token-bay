package admission

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueueEntry_EffectivePriority_NoWaitEqualsCredit(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	e := QueueEntry{CreditScore: 0.7, EnqueuedAt: now}
	assert.InDelta(t, 0.7, e.EffectivePriority(now, 0.05), 1e-9)
}

func TestQueueEntry_EffectivePriority_AgingBoost(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	e := QueueEntry{CreditScore: 0.3, EnqueuedAt: now.Add(-10 * time.Minute)}
	// 0.3 + 0.05 * 10 = 0.8
	assert.InDelta(t, 0.8, e.EffectivePriority(now, 0.05), 1e-9)
}

func TestQueueHeap_PopReturnsHighestEffectivePriority(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	h := newQueueHeap(now, 0.05)

	a := QueueEntry{ConsumerID: makeID(0xAA), CreditScore: 0.3, EnqueuedAt: now} // low credit, no wait
	b := QueueEntry{ConsumerID: makeID(0xBB), CreditScore: 0.7, EnqueuedAt: now} // high credit, no wait
	c := QueueEntry{ConsumerID: makeID(0xCC), CreditScore: 0.5, EnqueuedAt: now} // mid

	h.Push(a)
	h.Push(b)
	h.Push(c)

	got := []QueueEntry{h.Pop(), h.Pop(), h.Pop()}
	assert.Equal(t, b.ConsumerID, got[0].ConsumerID, "highest credit pops first")
	assert.Equal(t, c.ConsumerID, got[1].ConsumerID)
	assert.Equal(t, a.ConsumerID, got[2].ConsumerID)
}

func TestQueueHeap_AgingBoostOverridesStaticOrder(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	h := newQueueHeap(now, 0.05)

	// A: 0.3 credit, waited 10 min → effective 0.8
	// B: 0.5 credit, just enqueued → effective 0.5
	a := QueueEntry{ConsumerID: makeID(0xAA), CreditScore: 0.3, EnqueuedAt: now.Add(-10 * time.Minute)}
	b := QueueEntry{ConsumerID: makeID(0xBB), CreditScore: 0.5, EnqueuedAt: now}
	h.Push(a)
	h.Push(b)

	first := h.Pop()
	assert.Equal(t, a.ConsumerID, first.ConsumerID, "aged 0.3 outranks fresh 0.5 — admission-design §10 #6")
}

func TestQueueHeap_LenAndCap(t *testing.T) {
	h := newQueueHeap(time.Now(), 0.05)
	assert.Equal(t, 0, h.Len())
	h.Push(QueueEntry{ConsumerID: makeID(0xAA)})
	assert.Equal(t, 1, h.Len())
	h.Push(QueueEntry{ConsumerID: makeID(0xBB)})
	assert.Equal(t, 2, h.Len())
	h.Pop()
	assert.Equal(t, 1, h.Len())
}

func TestQueueHeap_PopFromEmpty_ReturnsZero(t *testing.T) {
	h := newQueueHeap(time.Now(), 0.05)
	z := h.Pop()
	assert.Equal(t, QueueEntry{}, z)
}

func TestQueueHeap_PeekDoesNotMutate(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	h := newQueueHeap(now, 0.05)
	h.Push(QueueEntry{ConsumerID: makeID(0xAA), CreditScore: 0.5, EnqueuedAt: now})
	h.Push(QueueEntry{ConsumerID: makeID(0xBB), CreditScore: 0.7, EnqueuedAt: now})

	first, ok := h.Peek()
	require.True(t, ok)
	assert.Equal(t, makeID(0xBB), first.ConsumerID)
	assert.Equal(t, 2, h.Len(), "Peek must not mutate")
}

func TestPopReadyForBroker_EmptyQueue(t *testing.T) {
	s, _ := openTempSubsystem(t)
	defer s.Close()
	_, ok := s.PopReadyForBroker(time.Now(), 0.0)
	require.False(t, ok)
}

func TestPopReadyForBroker_BelowMinPriority(t *testing.T) {
	s, _ := openTempSubsystem(t)
	defer s.Close()
	s.queueMu.Lock()
	s.queue.Push(QueueEntry{
		RequestID:   [16]byte{1},
		ConsumerID:  makeID(0xAA),
		CreditScore: 0.3,
		EnqueuedAt:  time.Now(),
	})
	s.queueMu.Unlock()
	_, ok := s.PopReadyForBroker(time.Now(), 0.5)
	require.False(t, ok)
}

func TestPopReadyForBroker_AbovePriority_Pops(t *testing.T) {
	s, _ := openTempSubsystem(t)
	defer s.Close()
	s.queueMu.Lock()
	s.queue.Push(QueueEntry{RequestID: [16]byte{1}, CreditScore: 0.8, EnqueuedAt: time.Now()})
	s.queueMu.Unlock()
	e, ok := s.PopReadyForBroker(time.Now(), 0.5)
	require.True(t, ok)
	require.Equal(t, [16]byte{1}, e.RequestID)
}

func TestPressureGauge_BootTime_ZeroComputedAt(t *testing.T) {
	s, _ := openTempSubsystem(t)
	defer s.Close()
	require.Zero(t, s.PressureGauge())
}

package admission

import (
	"container/heap"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// QueueEntry is one in-flight queued admission decision. Spec
// admission-design §4.2.
type QueueEntry struct {
	RequestID    [16]byte
	ConsumerID   ids.IdentityID
	EnvelopeHash [32]byte
	CreditScore  float64
	EnqueuedAt   time.Time
}

// EffectivePriority returns the heap-ordering key. CreditScore plus an
// aging boost proportional to wait time. Aging boost is what lets a low-
// credit consumer eventually serve under sustained pressure
// (admission-design §5.4).
func (e *QueueEntry) EffectivePriority(now time.Time, agingAlpha float64) float64 {
	wait := now.Sub(e.EnqueuedAt)
	if wait < 0 {
		wait = 0
	}
	waitMinutes := wait.Minutes()
	return e.CreditScore + agingAlpha*waitMinutes
}

// queueHeap is a max-heap of QueueEntry by EffectivePriority. The heap
// captures (now, agingAlpha) at construction time; the Less function
// uses those for the ordering. Push/Pop/Peek are typed wrappers over
// container/heap.
type queueHeap struct {
	entries    []QueueEntry
	now        time.Time
	agingAlpha float64
}

func newQueueHeap(now time.Time, agingAlpha float64) *queueHeap {
	return &queueHeap{now: now, agingAlpha: agingAlpha}
}

func (h *queueHeap) Len() int { return len(h.entries) }
func (h *queueHeap) Less(i, j int) bool {
	pi := h.entries[i].EffectivePriority(h.now, h.agingAlpha)
	pj := h.entries[j].EffectivePriority(h.now, h.agingAlpha)
	return pi > pj // max-heap
}
func (h *queueHeap) Swap(i, j int)  { h.entries[i], h.entries[j] = h.entries[j], h.entries[i] }
func (h *queueHeap) heapPush(x any) { h.entries = append(h.entries, x.(QueueEntry)) }
func (h *queueHeap) heapPop() any {
	n := len(h.entries) - 1
	x := h.entries[n]
	h.entries = h.entries[:n]
	return x
}

// Push adds an entry to the heap.
func (h *queueHeap) Push(e QueueEntry) {
	heap.Push((*queueHeapAdapter)(h), e)
}

// Pop returns the top entry. Returns the zero value when the heap is
// empty (callers MUST check Len() == 0 before calling if they need
// distinguishability).
func (h *queueHeap) Pop() QueueEntry {
	if len(h.entries) == 0 {
		return QueueEntry{}
	}
	return heap.Pop((*queueHeapAdapter)(h)).(QueueEntry)
}

// Peek returns the top entry without removing it. ok = false on an
// empty heap.
func (h *queueHeap) Peek() (QueueEntry, bool) {
	if len(h.entries) == 0 {
		return QueueEntry{}, false
	}
	return h.entries[0], true
}

// AdvanceTime updates the "now" cached on the heap. Re-ordering is NOT
// done automatically — callers needing strict ordering after a clock
// jump should call Reheapify().
func (h *queueHeap) AdvanceTime(now time.Time) { h.now = now }

// Reheapify reorders the heap. Useful after AdvanceTime when the aging
// boost has shifted some entries' effective priorities relative to others.
func (h *queueHeap) Reheapify() {
	heap.Init((*queueHeapAdapter)(h))
}

// queueHeapAdapter exposes queueHeap as a heap.Interface implementation
// without forcing queueHeap itself to expose Push/Pop in the heap.Interface
// shape (which would conflict with our Push/Pop methods).
type queueHeapAdapter queueHeap

func (a *queueHeapAdapter) Len() int           { return (*queueHeap)(a).Len() }
func (a *queueHeapAdapter) Less(i, j int) bool { return (*queueHeap)(a).Less(i, j) }
func (a *queueHeapAdapter) Swap(i, j int)      { (*queueHeap)(a).Swap(i, j) }
func (a *queueHeapAdapter) Push(x any)         { (*queueHeap)(a).heapPush(x) }
func (a *queueHeapAdapter) Pop() any           { return (*queueHeap)(a).heapPop() }

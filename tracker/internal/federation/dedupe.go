package federation

import (
	"container/list"
	"sync"
	"time"
)

type dedupeEntry struct {
	id     [32]byte
	expiry time.Time
}

// Dedupe is a TTL-bounded set keyed by message_id. Insertion order is
// tracked via a doubly-linked list to support O(1) eviction of the
// oldest entry when capacity is hit.
type Dedupe struct {
	ttl time.Duration
	cap int
	now func() time.Time

	mu    sync.Mutex
	order *list.List // front = newest
	idx   map[[32]byte]*list.Element
}

func NewDedupe(ttl time.Duration, capacity int, now func() time.Time) *Dedupe {
	if capacity <= 0 {
		capacity = 1024
	}
	if now == nil {
		now = time.Now
	}
	return &Dedupe{ttl: ttl, cap: capacity, now: now, order: list.New(), idx: make(map[[32]byte]*list.Element)}
}

// Mark records id with expiry = now + ttl. Idempotent — re-marking refreshes the TTL.
func (d *Dedupe) Mark(id [32]byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.evictExpiredLocked()
	if el, ok := d.idx[id]; ok {
		el.Value.(*dedupeEntry).expiry = d.now().Add(d.ttl)
		d.order.MoveToFront(el)
		return
	}
	for d.order.Len() >= d.cap {
		oldest := d.order.Back()
		if oldest == nil {
			break
		}
		ent := oldest.Value.(*dedupeEntry)
		delete(d.idx, ent.id)
		d.order.Remove(oldest)
	}
	el := d.order.PushFront(&dedupeEntry{id: id, expiry: d.now().Add(d.ttl)})
	d.idx[id] = el
}

// Seen returns true iff id is currently present (TTL not expired).
func (d *Dedupe) Seen(id [32]byte) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.evictExpiredLocked()
	_, ok := d.idx[id]
	return ok
}

// Size returns the current number of live entries (test-only convenience).
func (d *Dedupe) Size() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.evictExpiredLocked()
	return d.order.Len()
}

func (d *Dedupe) evictExpiredLocked() {
	now := d.now()
	for {
		el := d.order.Back()
		if el == nil {
			return
		}
		ent := el.Value.(*dedupeEntry)
		if now.Before(ent.expiry) {
			return
		}
		delete(d.idx, ent.id)
		d.order.Remove(el)
	}
}

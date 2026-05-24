package broker

import (
	"context"
	"time"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// RegisterQueued caches an envelope and a delivery callback for a request_id
// that admission has queued. When the queue-drain goroutine pops the request
// from admission, it calls Submit with this cached envelope and invokes
// `deliver` with the result.
func (b *Broker) RegisterQueued(env *tbproto.EnvelopeSigned, requestID [16]byte, deliver func(*Result)) {
	if env == nil || env.Body == nil {
		return
	}
	b.pendingMu.Lock()
	defer b.pendingMu.Unlock()
	ch := make(chan *Result, 1)
	b.pendingQueued[requestID] = pendingEnv{body: env.Body, deliver: ch}
	go func() {
		r, ok := <-ch
		if ok && r != nil {
			deliver(r)
		}
	}()
}

// CancelQueued drops the pendingQueued entry for requestID. Used by the api
// layer's block-then-deliver path when its own timer or the consumer's ctx
// fires before drain reaches the entry — without this, drain would later
// Submit on behalf of a consumer that has already received a wire response,
// burning a reservation and (worst case) an offer-accept round-trip. Idempotent.
//
// Races against drainOnce are intentional: if drain has already popped the
// entry from pendingQueued, CancelQueued is a no-op and the Submit proceeds
// to completion. The reservation reaper backstops that case.
func (b *Broker) CancelQueued(requestID [16]byte) {
	b.pendingMu.Lock()
	p, exists := b.pendingQueued[requestID]
	if exists {
		delete(b.pendingQueued, requestID)
	}
	b.pendingMu.Unlock()
	if exists {
		close(p.deliver)
	}
}

// TriggerQueueDrain forces one drain iteration. The drain goroutine also
// fires periodically per cfg.QueueDrainIntervalMs.
func (b *Broker) TriggerQueueDrain() {
	select {
	case b.queueDrainCh <- struct{}{}:
	default:
	}
}

func (b *Broker) startQueueDrain() {
	interval := time.Duration(b.cfg.QueueDrainIntervalMs) * time.Millisecond
	if interval <= 0 {
		interval = time.Second
	}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-b.stop:
				return
			case <-t.C:
				b.drainOnce(context.Background())
			case <-b.queueDrainCh:
				b.drainOnce(context.Background())
			}
		}
	}()
}

// drainOnce pops every queue entry currently ready and re-enters Submit for
// each. Each delivery channel is sent-to (1-buffered) and then closed so the
// RegisterQueued goroutine returns.
func (b *Broker) drainOnce(ctx context.Context) {
	pressure := b.deps.Admission.PressureGauge()
	minPriority := 0.0
	if pressure > 1.0 {
		minPriority = 0.5 * (pressure - 1.0)
	}
	now := b.deps.Now()
	for {
		entry, ok := b.deps.Admission.PopReadyForBroker(now, minPriority)
		if !ok {
			return
		}
		b.pendingMu.Lock()
		p, exists := b.pendingQueued[entry.RequestID]
		if exists {
			delete(b.pendingQueued, entry.RequestID)
		}
		b.pendingMu.Unlock()
		if !exists {
			continue
		}
		signed := &tbproto.EnvelopeSigned{Body: p.body}
		result, err := b.Submit(ctx, signed)
		if err != nil {
			result = &Result{Outcome: OutcomeNoCapacity, NoCap: &NoCapacityDetails{Reason: "broker_error"}}
		}
		select {
		case p.deliver <- result:
		default:
		}
		close(p.deliver)
	}
}

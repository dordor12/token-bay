// Package admission gates broker_request on regional supply pressure and a
// composite per-consumer credit score. It sits in front of the broker per
// admission-design §1: the broker still picks which seeder serves a request,
// but admission decides whether to run the broker at all (ADMIT), defer
// (QUEUE), or refuse (REJECT).
//
// State is in-memory only in this revision: ConsumerCreditState (rolling
// 30-day-bucket histories of settlements / disputes / flow), SeederHeartbeat-
// State (10-minute rolling reliability buckets), and an atomically-published
// SupplySnapshot derived from the registry every 5 seconds. Persistence —
// admission.tlog, periodic snapshots, StartupReplay — lands in a follow-up
// plan; this revision does not write to disk.
//
// The ledger emits no events to admission yet either: this revision exposes
// LedgerEventObserver as the consumer-side contract, and tests drive it
// directly with synthetic events. Real wiring (ledger → admission event bus)
// is a separate plan.
//
// Concurrency model: per-consumer and per-seeder state is sharded; each
// shard owns its own sync.RWMutex. The hot path Decide reads the supply
// snapshot via atomic.Pointer.Load (no lock) and a single shard's RLock
// for the credit-state lookup. The queue is a max-heap protected by one
// mutex; queue ops are never on the Decide-frequency hot path. The 5s
// aggregator goroutine writes the supply pointer atomically; readers
// always observe a consistent snapshot.
//
// Spec: docs/superpowers/specs/admission/2026-04-25-admission-design.md.
package admission

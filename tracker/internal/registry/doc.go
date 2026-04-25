// Package registry holds the tracker's live, in-memory seeder registry.
//
// A SeederRecord captures everything the broker needs to pick a candidate
// for a consumer's request: the seeder's identity, what it can serve
// (Capabilities), how busy it is (Load), how recently we heard from it
// (LastHeartbeat), and where to reach it (NetCoords). The registry is a
// pure in-memory store; persistence is intentionally out of scope.
//
// Concurrency model: the registry shards by hash(IdentityID) into a fixed
// number of shards (configurable, default 16). Each shard owns its own
// sync.RWMutex. Read-heavy paths (Get, Snapshot, Match) take RLocks per
// shard; mutators take the write lock on a single shard. There is no
// global lock, so contention scales with the number of shards.
//
// All readers return value copies of SeederRecord (and the slices it
// contains). Callers cannot mutate the store through a returned record.
//
// The registry intentionally does not know about: ledger state, reputation
// freezing, broker scoring, network I/O, or persistence. Those concerns
// live in their respective tracker modules.
//
// Spec: docs/superpowers/specs/tracker/2026-04-22-tracker-design.md §3, §4.1, §5.1, §6.
package registry

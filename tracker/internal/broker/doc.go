// Package broker is the tracker subsystem that, after admission has admitted
// a broker_request, picks a capable seeder via the offer round-trip and later
// finalizes the request as a USAGE ledger entry.
//
// Broker is two sibling subsystems sharing in-memory state:
//   - *Broker     — selection: validates pricing, reserves credits, scores
//                   candidates from the registry, runs the offer push loop.
//                   Exposes Submit(ctx, env) → *Result.
//   - *Settlement — finalization: receives usage_report from the seeder,
//                   pushes settle_request to the consumer, awaits the
//                   counter-sig (or times out → ConsumerSigMissing entry),
//                   appends via internal/ledger.
//
// The two share *Inflight + *Reservations. Construct via Open which returns
// a *Subsystems composite; cmd/run_cmd threads each into api.Deps.
//
// Concurrency: registry shards + per-mutex on Inflight/Reservations + CAS
// state transitions. Race-clean tests are mandatory (this package is on
// the always-`-race` list per tracker spec §6).
//
// Spec: docs/superpowers/specs/tracker/2026-05-07-tracker-broker-design.md.
package broker

# Tracker — open gaps

| Field | Value |
|---|---|
| Date | 2026-05-11 |
| Scope | Outstanding wiring / correctness / metric gaps in the `tracker/` module after rebase onto `main` at `e80e629` |
| Method | Cross-checked specs in `docs/superpowers/specs/{tracker,ledger,federation,reputation,admission,tee-tier,exhaustion-proof}/` against `tracker/` source; ten parallel deep-dive audits |

## Recently closed (for context)

| Gap | Closed by | Evidence |
|---|---|---|
| Hourly Merkle root rollup orchestrator | #44 | `tracker/cmd/token-bay-tracker/run_cmd.go:77` `led.StartRollup(...)` |
| `Federation.PublishHour` ticker | #43 | `tracker/cmd/token-bay-tracker/maintenance.go:76` |
| `Federation.PublishPeerExchange` ticker | #43 | `maintenance.go:80` |
| Admission snapshot emitter goroutine | #43 | `tracker/internal/admission/admission.go:192` |
| `transfer_request` RPC end-to-end | #46 | `transferFederationAdapter` injected at `run_cmd.go:256` |
| Broker queue block-then-deliver | #48 | `tracker/internal/api/broker_request.go:153` calls `RegisterQueued` |
| Admin `/peers` + add/remove | #45 | `tracker/internal/admin/handlers.go:51,96,150` |
| Registry stale-heartbeat sweeper | #43 | `maintenance.go:111` `reg.Sweep(...)` |
| STUN/TURN session sweeper | #43 | `maintenance.go:133` `alloc.Sweep(now)` |
| Local `reputation.IsFrozen` gate on `broker.Submit` | #47 | `tracker/internal/broker/broker.go:128` |
| TURN relay seeder-identity lookup | #49 | `turnAssignmentLookup` interface in `tracker/internal/api/turn_relay_open.go` |
| Config knobs `PublishCadenceS` / `SnapshotIntervalS` / `MerkleRootIntervalMin` consumed | #43, #44 | maintenance loops + `StartRollup` |

## Open — correctness / spec-mandated

### T8 — Settlement consumer-sig verification (T17.5)

- **Where:** `tracker/internal/broker/settlement.go:150,244`
- **Symptom:** Every appended usage / settlement entry has `ConsumerSigMissing = true`.
- **Why deferred:** Spec §5.2 step 11 requires verifying the consumer signature on `UsageReport` before append. The wire format binds the seeder signature over a body that *includes* `ConsumerSigMissing`, so flipping it after verification would invalidate the seeder sig (see `settlement.go:196-206`). Needs a wire-format amendment.
- **Plan:** Define amendment in `shared/proto/`, plumb `deps.Identity.PeerPubkey(consumer)` resolver into `HandleSettle`, verify sig, set `ConsumerSigMissing=false` before `AppendUsage`. Coordinate with `shared/` callers.

### T9 — `Inflight.Transition` ignores injected clock

- **Where:** `tracker/internal/session/inflight.go:54`
- **Symptom:** `r.TerminatedAt = time.Now()` is hard-coded. The session subsystem and `AllocatorConfig` honor an injected clock elsewhere.
- **Impact:** Tests cannot deterministically exercise terminated-at; production clock skew goes unobserved.
- **Plan:** Change signature to `Transition(id, from, to, now time.Time)` (or add a `nowFn` field on `Inflight`); update call sites in `broker/settlement.go` and `session/manager.go`.

### T10 — `Ledger.AssertChainIntegrity` never invoked at runtime

- **Where:** `tracker/internal/ledger/audit.go:23`
- **Symptom:** All 12 callers are in `*_test.go`. Spec §8 acceptance ("hash(entry[n-1]) == entry[n].prev_hash ∀ n") is unverified in production.
- **Plan:** Add a startup pre-flight in `run_cmd.go` that calls `led.AssertChainIntegrity(ctx, 0, tip.Seq)` before serving traffic; consider a periodic background audit at a low cadence (e.g. once per `MerkleRootIntervalMin`).

### T13 — Settlement pre-transition state guard missing

- **Where:** `tracker/internal/broker/settlement.go:120-125`
- **Symptom:** Handler attempts `Transition(ASSIGNED→SERVING)` and silently swallows `ErrIllegalTransition`. Spec tracker-broker-design §5.2 step 2 requires explicit rejection when `req.State ∉ {ASSIGNED, SERVING}` with `RPC_STATUS_INVALID code INVALID_STATE`.
- **Impact:** `usage_report` for sessions in `SELECTING` / `FAILED` leak through to downstream cost / overspend checks.
- **Plan:** Read current state via `Inflight.Get`; reject explicitly outside the allowed set; only then attempt the transition.

## Open — observability

### T19 — `admission.FetchHeadroomToS` metric has zero `.Inc()` callers

- **Where:** `tracker/internal/admission/metrics.go:97`
- **Symptom:** Counter is registered (`metrics.go:175`) but never emitted.
- **Plan:** Either wire the counter at the `FetchHeadroom` boundary (likely `tracker/internal/api/advertise.go` or wherever the heartbeat is processed), or delete the metric definition.

### T20 — `broker.SeederPostAcceptDisconnect` metric has zero `.Inc()` callers

- **Where:** `tracker/internal/broker/metrics.go:30,100`
- **Symptom:** Registered (`metrics.go:145`) but never emitted.
- **Plan:** Wire at the seeder-disconnect-after-accept code path (likely in `broker/offer_loop.go` when a push-stream close happens after `Result.Accept`), or delete.

## Open — choke-point / invariant

### M4 — `CanonicalBootstrapPeerListPreSig` bypasses `signing.DeterministicMarshal`

- **Where:** `shared/proto/canonical.go:37` (used by tracker bootstrap-peers handler)
- **Symptom:** Calls `proto.MarshalOptions{Deterministic: true}.Marshal()` directly. Every other canonical helper routes through `signing.DeterministicMarshal`; spec §4.1 mandates a single choke point.
- **Plan:** Route through `signing.DeterministicMarshal`; rerun golden tests to confirm bytes unchanged.

## Open — documented MVP scope (lower priority)

| ID | Gap | Location | Note |
|---|---|---|---|
| T21 | TEE attestation PCR-whitelist validation | `tracker/internal/registry/record.go:16` | Spec marked "needs redesign" |
| T22 | Reputation signal derivation: `completion_health`, `consumer_satisfaction`, `seeder_fairness` default to 1.0 | `tracker/internal/reputation/evaluator.go:236-238` | MVP scope (spec §7 bounded TODO) |
| T23 | Reputation `FirstSeenAt` never decays on dormancy | (no decay path) | Open question §10.4 — exploitable account-takeover edge |
| T24 | Reputation evaluator recomputes every identity per cycle | `evaluator.go:251-272` | Perf only; correctness unaffected |

## Priority ladder

1. **T13** — settlement state guard (silent spec violation)
2. **T19 / T20** — wire `.Inc()` callers or delete dead metrics
3. **T8 (T17.5)** — consumer-sig verification (wire-format amendment)
4. **M4** — `DeterministicMarshal` choke-point invariant
5. **T9** — `Inflight.Transition` clock injection
6. **T10** — `AssertChainIntegrity` startup pre-flight
7. **T21–T24** — MVP follow-ups

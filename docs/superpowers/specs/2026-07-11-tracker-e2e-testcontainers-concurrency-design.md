# Tracker E2E: testcontainers migration + concurrent-actor matrix — Design

**Date:** 2026-07-11
**Status:** Approved (design phase)
**Builds on:** `docs/superpowers/specs/2026-07-08-tracker-e2e-testing-design.md` (the existing 20-scenario Docker e2e suite).

## Goal

Two coupled changes to the tracker e2e suite:

1. **Migrate the harness from `docker compose` shell-out to testcontainers-go**, so container lifecycle is programmatic (dynamic containers, automatic port mapping, typed wait strategies) — while preserving all 20 existing scenarios green.
2. **Add concurrent multi-consumer/multi-seeder scenarios** covering the meaningful combinations of happy and non-happy flows, to stress the tracker's concurrency and prove correctness (per-request outcome + ledger conservation) under load.

## Non-goals

- Not changing any tracker/plugin **product** code except two small **actor** (test-fixture) additions (seeder over-report knob, consumer overspend behavior).
- Not stress-testing STUN/TURN NAT traversal (out of scope, as before).
- Not targeting the spec's ≥100-conn ceiling — a CI-bounded concurrency (~18 concurrent requests) that proves correctness, not a throughput benchmark.

## Part 1 — testcontainers harness migration

### Architecture

A new `driver.Stack` type (`tracker/test/e2e/driver/stack.go`, `//go:build e2e`) owns the topology via `github.com/testcontainers/testcontainers-go` (added test-only to `tracker/go.mod`):

- Creates one Docker **network** with aliases `tracker-a`, `tracker-b`, `seeder`, `consumer`, `fedactor` — matching the hostnames the generated configs already reference (`tracker-b:7443`, `seeder:7900`, etc.).
- Starts the two tracker containers (pre-built image `token-bay-tracker:dev`) and the base actor/fedactor containers (`tokenbay-e2e-actors:dev`), each with: the `.gen` files mounted (bind or `Files:` copy), env (`TOKEN_BAY_ADMIN_TOKEN`), the same `command` args as `compose.e2e.yaml`.
- Waits on readiness with testcontainers `wait` strategies: trackers on an HTTP `/health` 200 (with the bearer header) or the "admin: listening" log line; actors on `/healthz` 200 or the "connected" log line.
- Exposes each container's **mapped host port** (`MappedPort`) so the driver's HTTP/QUIC clients target `localhost:<mapped>` instead of fixed ports.
- Provides `Exec(svcAlias, cmd...)`, `Logs(svcAlias)`, `Restart(svcAlias)`, and `Terminate()` mirroring the current `driver.Compose` surface, so scenario code is source-compatible.

Images remain **pre-built** by `make test-e2e` (testcontainers references them by tag — no `FromDockerfile`, which would rebuild every run). `e2egen` still generates `.gen` in `TestMain`.

`compose.e2e.yaml` is **kept** as a manual-debugging convenience (`docker compose up`), but is no longer the test path. To prevent drift, the container definitions in `driver.Stack` are the single test source of truth; a short comment cross-references the compose file.

### Migration mechanics

- `TestMain` (`main_test.go`) replaces `compose().Up()/Down()` with `stack.Start(ctx)` / `t.Cleanup(stack.Terminate)`. It still runs `e2egen` first.
- Package-level accessors (`adminA()`, `adminB()`, `consumerCtl()`, `seederCtl()`, `fedactorCtl()`, `compose()`) are re-pointed at the `Stack` (the last renamed to `stack()`), returning clients built from mapped ports.
- Scenario bodies (1-20) are otherwise unchanged. `SQLiteQuery`/`Logs`/`Restart` route through `Stack.Exec/Logs/Restart`.
- **Validation gate:** all 20 existing scenarios must pass green on the testcontainers harness before any concurrency work begins. This is the migration's acceptance test.

### Error handling

- Any container that fails its wait strategy fails `TestMain` fast with its logs attached.
- `Terminate()` runs on cleanup even on panic (via `t.Cleanup` / defer), removing the network + containers + volumes; no orphaned resources.
- Mapped-port lookups that fail surface a clear error naming the service.

## Part 2 — concurrent-actor behavior matrix

### Model-based routing (deterministic pairing under concurrency)

The broker selects the seeder, so to pin consumer→seeder pairings while running concurrently, each seeder **behavior advertises a distinct priced model**, and each consumer **requests the model** for its intended target:

- `claude-sonnet-4-6` → **happy** seeders
- `claude-opus-4-7` → **over-reporting** seeder
- `claude-haiku-4-5-20251001` → **unavailable** seeder

All three are already in the pricing table. The broker's model filter routes each consumer to the intended seeder-behavior class deterministically, even with everything in flight at once.

### Actor additions (test-fixture only)

1. **Seeder `over-report`** — a `SeederConfig` knob (e.g. `UsageInflate float64` or a boolean `OverReport`) that makes the seeder's `UsageReport` claim tokens exceeding the offer's max, so the tracker's overspend guard rejects it (`COST_OVERSPEND`). Default off (existing behavior unchanged).
2. **Consumer `overspend`** — a request behavior where `MaxInputTokens`/`MaxOutputTokens` are chosen so `MaxCost > the consumer's balance`, triggering the broker's `insufficient_credits` rejection. Expressed via the existing `RequestSpec` (no new field needed if the scenario computes oversized token counts) or a `ConsumerConfig` flag.

`dispute` (no settle) and `abandoned` (no dial) reuse the existing `ConsumerConfig.Settle`/`Dial` toggles. `unavailable` reuses `SeederConfig.Available=false`.

### The matrix (six meaningful combinations)

| Consumer × Seeder | Model | Expected terminal state |
|---|---|---|
| happy × happy | sonnet | completed USAGE entry, both sigs, balances move |
| dispute × happy | sonnet | `consumer_sig_missing` entry, balances move |
| abandoned × happy | sonnet | reaped → `failed`, no entry |
| happy × over-report | opus | `COST_OVERSPEND` — no entry, no debit |
| happy × unavailable | haiku | `NoCapacity` — no assignment |
| overspend × happy | sonnet | `insufficient_credits` — rejected at broker |

### Scale (parameterized, CI-bounded)

Defaults: **4 happy** seeders + **1 over-report** + **1 unavailable** (6 seeder containers); **~18 concurrent consumer** requests spread across the six combinations (≈3 each). `N`/`M` are constants at the top of the scenario file, easy to bump for heavier local runs.

### Concurrency execution

The scenario (`concurrency_test.go`, scenarios 21+):
1. Spawns the seeder containers via `Stack`, each configured with its behavior + advertised model; waits for each to advertise (poll admin `/identity/{seeder}` until available/headroom set).
2. Spawns the consumer containers, each with its behavior + target model.
3. Fires all consumer requests **concurrently** (one goroutine per consumer, a shared start barrier), with a bounded overall timeout.
4. Collects per-worker `RequestResult` + settlement outcome.

### Assertions

1. **Per-worker correctness** — each request's terminal state equals its `(consumer-behavior, seeder-behavior)` expectation. No cross-talk: each USAGE/dispute entry references the correct `consumer_id`/`seeder_id`/`request_id` (verified by SQL join, not just counts).
2. **Ledger conservation** — over the whole batch: `Σ consumer debits == Σ seeder credits` for all settled+disputed requests; total credits == starter grants − net (no creation/destruction); chain integrity passes (`AssertChainIntegrity`-equivalent via the startup gate or a direct check); no duplicate `seq`, no gaps.
3. **Liveness** — the entire concurrent batch reaches terminal states within the bounded timeout (no deadlock/starvation in broker matching or ledger append serialization).

### Cross-scenario isolation

The concurrency scenario mints its **own dedicated consumer + seeder identities** (fresh containers), so it does not perturb the balances/state the 1-20 scenarios rely on, and vice-versa. It reads the trackers' state but adds only its own entries.

## Testing / acceptance

- **Migration:** all 20 existing scenarios pass on testcontainers (the migration acceptance gate).
- **Concurrency:** the matrix scenario passes with all per-worker outcomes correct + ledger conserved + within timeout, across ≥3 consecutive runs (flake check).
- `make test-e2e` runs the whole suite (1-20 + concurrency) via testcontainers; the CI `e2e` job runs it on ubuntu.
- `go vet -tags=e2e` + `golangci-lint --build-tags e2e` clean; untagged `go build ./...` unaffected; `go mod tidy` keeps the new dep minimal and direct.

## Risks

- **testcontainers migration under a green suite** — the lifecycle/networking wiring is the risk; mitigated by the all-20-green gate before concurrency work.
- **New heavyweight test dependency** (`testcontainers-go` + Docker client tree) in `tracker/go.mod` — accepted per the decision; kept test-only.
- **Concurrency flakiness** — bounded scale + condition-polling + a ≥3-run flake check; any real race surfaced is a genuine tracker bug to root-cause (the point of the exercise).

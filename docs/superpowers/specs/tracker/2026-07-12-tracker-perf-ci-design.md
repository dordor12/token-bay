# Tracker Performance CI — Design

**Date:** 2026-07-12
**Status:** Accepted
**Related:** `2026-07-08-tracker-e2e-testing-design.md` (§10 deferred perf as separate work),
`2026-07-11-tracker-e2e-testcontainers-concurrency-design.md` (testcontainers harness this builds on)

## 1. Problem

The e2e suite proves correctness at CI-bounded scale (~18 concurrent requests, 2 trackers,
container-per-actor). Nothing exercises the tracker under sustained load: thousands of
concurrent consumer/seeder connections, a growing federation, an hour of continuous
broker/settlement traffic. Regressions in broker latency, settlement throughput, registry
sharding, or federation gossip under load are invisible until deployment.

## 2. Goals / non-goals

**Goals**

- A soak/load harness that drives **10,000 simulated consumers and 10,000 simulated
  seeders** (defaults; env-tunable) against a fleet of real tracker containers over the
  real QUIC wire protocol.
- **Federation growth under load**: the run starts with 2 federated trackers and keeps
  adding tracker containers to the network on an interval while load is running.
- A **duration parameter** (default **1 hour**) so the same pipeline serves quick smokes
  and full soaks.
- A CI pipeline that runs **only on PRs labeled `core`** (plus manual `workflow_dispatch`),
  fails on hard health/error thresholds, and always publishes a metrics report.

**Non-goals**

- Tunnel data-path load. The tracker is control-path only (tracker rule 1); the
  consumer↔seeder tunnel never touches it, so simulated participants skip the tunnel
  entirely: the seeder accepts the offer and reports usage as if it had served.
- Micro-benchmarks (covered by `internal/admission/perf_bench_test.go` style tests).
- Byzantine behavior under load (e2e covers Byzantine correctness).

## 3. Scale model — why simulated in-process participants

10k containers is impossible on a CI runner (4 vCPU / 16 GB). Participants are therefore
goroutine-hosted simulated clients in the `go test` process, each with:

- its own throwaway Ed25519 identity and mTLS QUIC connection (real handshake, real
  identity derivation — `sha256(DER SPKI)`);
- the real wire protocol end to end: ENROLL, ADVERTISE, heartbeat pings, BALANCE,
  consumer-signed `EnvelopeSigned` BROKER_REQUEST, server-initiated offer push
  (tag `0x01`) answered with an ephemeral-key `OfferDecision`, ephemeral-key-signed
  USAGE_REPORT, settlement push (tag `0x02`) counter-signed via SETTLE + `SettleAck`.

QUIC connections share a bounded pool of UDP sockets (`quic.Transport` per socket,
round-robin) so 20k connections don't need 20k file descriptors.

**Sustainability arithmetic:** consumers request the default model
(`claude-haiku-4-5-20251001`, 1 in + 5 out credits/token) with 1/1 tokens = 6 credits per
settled request, paced at one request per `PERF_REQUEST_INTERVAL` (default 60 s). Worst
case (a consumer alive the whole hour) spends ~360 credits — inside the 1000-credit
starter grant, so the load is sustainable without credit top-ups. A consumer whose
balance drops below one request's max cost stops and counts `budget_exhausted` (not an
error).

**Ramp:** participants spawn at a uniform rate over the first 80% of the run (interleaved
seeder/consumer so capacity grows with demand), each attaching round-robin to the tracker
fleet *live at spawn time* — which is how newly-joined trackers organically receive load.
The last 20% runs at full population.

## 4. Tracker fleet growth

- All `PERF_TRACKERS_MAX` (default 8) tracker identities and YAML configs are rendered
  up-front (deterministic seeds, full-mesh `federation.peers` allowlists — the validator
  requires static allowlists, and dialing a not-yet-started peer just redials with
  backoff).
- `PERF_TRACKERS_INITIAL` (default 2) containers start before load; one more starts every
  `PERF_TRACKER_ADD_INTERVAL` (default 5 m) until the cap.
- Containers are `token-bay-tracker:dev` (same image as e2e) managed via
  testcontainers-go `GenericContainer` on one Docker network, with mapped host ports for
  QUIC RPC (udp), admin, and metrics.
- Config mirrors e2egen's e2e tuning, with settlement windows loosened for load
  (settlement_timeout_s=10, reservation_ttl_s=15, offer_timeout_ms=2000);
  `publish_cadence_s=60` (validator floor) gives real gossip traffic over an hour.

## 5. Harness layout

```
tracker/test/perf/
  main_test.go        //go:build perf — TestMain: params, fleet up, teardown
  perf_test.go        //go:build perf — the soak scenario + threshold assertions
  gen.go              //go:build perf — N-tracker config rendering (reuses internal/config)
  fleet.go            //go:build perf — testcontainers fleet manager + staggered joins
  loadgen/            //go:build perf — SimConsumer, SimSeeder, transport pool, counters
  report/             (no build tag) — params, prometheus text parsing, thresholds,
                      JSON + markdown report rendering; unit-tested by `make test`
```

`tracker/test/e2e/driver` is reused for `RPCClient`/`Admin` (build constraint widened to
`e2e || perf`), extended with: dial-via-shared-`quic.Transport`, an `AcceptStream`/frame
API for client-side push handling, and heartbeat send.

## 6. Measurements and hard thresholds

Client-side counters (atomics in loadgen) + a 15 s scrape loop over every live tracker's
`/metrics` and admin `/stats`. The run **fails** when (env-overridable):

| Threshold | Default |
|---|---|
| Tracker container dies or `/health` != ok at end | any ⇒ fail |
| Transport/dial/unexpected-RPC-status errors ÷ total operations | > 1% ⇒ fail |
| BROKER_REQUEST client-observed p99 | > 5 s ⇒ fail |
| Settlements counter-signed ÷ usage reports sent | < 95% ⇒ fail |
| Duplicate reservation tokens (double-assign) | any ⇒ fail |
| Federation steady-peer edges ÷ expected full-mesh edges at end | < 80% ⇒ fail |

Queued / no-capacity / budget-exhausted outcomes are *valid* (admission doing its job),
reported but never failing.

The report (JSON + markdown) lands in `PERF_REPORT_DIR`: outcome breakdown, latency
percentiles, per-tracker broker/admission/federation metric finals, threshold verdicts.
CI uploads it as an artifact and appends the markdown to the job summary.

## 7. Parameters

All env vars, read by `report.ParamsFromEnv`:

| Var | Default | Meaning |
|---|---|---|
| `PERF_DURATION` | `1h` | total run length (Go duration) |
| `PERF_CONSUMERS` / `PERF_SEEDERS` | `10000` / `10000` | population |
| `PERF_TRACKERS_INITIAL` / `PERF_TRACKERS_MAX` | `2` / `8` | fleet size |
| `PERF_TRACKER_ADD_INTERVAL` | `5m` | federation growth cadence |
| `PERF_REQUEST_INTERVAL` | `60s` | per-consumer pacing |
| `PERF_SOCKETS` | `32` | UDP socket pool size |
| `PERF_SCRAPE_INTERVAL` | `15s` | metrics sampling |
| `PERF_REPORT_DIR` | `perf-report` | report output |
| `PERF_MODEL` | `claude-haiku-4-5-20251001` | requested model |
| threshold overrides | §6 defaults | `PERF_MAX_ERR_RATE`, `PERF_MAX_BROKER_P99`, `PERF_MIN_SETTLE_RATE`, `PERF_MIN_FED_STEADY` |

## 8. CI pipeline (`.github/workflows/perf.yml`)

- **Triggers:** `pull_request` (types `opened, synchronize, reopened, labeled`) gated by
  `contains(github.event.pull_request.labels.*.name, 'core')`; and `workflow_dispatch`
  with a `duration` input (default `1h`) plus optional scale inputs.
- One run per PR at a time (`concurrency` group, cancel-in-progress) — an hour-long job
  must not pile up behind every push.
- ubuntu-latest only (Docker), UDP buffer sysctls raised (quic-go), `ulimit -n 65536`,
  job `timeout-minutes` comfortably above the longest expected run.
- `make -C tracker test-perf` with the parameter env; report artifact always uploaded;
  markdown appended to `$GITHUB_STEP_SUMMARY`; tracker container logs uploaded on
  failure.

The `core` label itself is repository metadata (create once in GitHub settings); the
workflow simply no-ops the job for unlabeled PRs.

## 9. Determinism / flake posture

This is a soak test, not a correctness gate replay: thresholds are generous floors meant
to catch collapses (crash, deadlock, error storm, federation partition), not small
latency drift. Tracker identity seeds are deterministic; participant identities are
throwaway-random (uniqueness is what matters). The run tolerates individual client
errors up to the error budget — a single flaky dial never fails the pipeline.

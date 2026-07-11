# Tracker End-to-End Testing (Docker) — Design

**Date:** 2026-07-08
**Status:** Draft for review
**Scope decision (from user):** Full-fix including cross-region transfer. This effort both *fixes* the tracker's real end-to-end gaps and *proves* correctness with a Docker-based e2e suite.

---

## 1. Goal & motivation

Verify that the `token-bay-tracker` component is **completely working and ready to deploy** by standing up a realistic, multi-node Token-Bay network in Docker and driving real protocol flows end-to-end:

- **2 real regional tracker containers** (region-A "home", region-B "peer"), federated over QUIC.
- A **controllable consumer actor** and a **controllable seeder actor** (no `claude` binary, no Anthropic key) that speak the real plugin↔tracker wire protocol.
- A **Byzantine federation actor** ("fedactor") that impersonates a neighbor tracker to exercise adversarial federation paths (equivocation, malicious revocation).
- A **Go test driver** that orchestrates the topology and asserts outcomes via the admin HTTP API, the BALANCE RPC, ledger SQLite inspection, and container logs.

### Why this is more than a test harness

Verification against the current source (four exploration passes + an eight-way adversarial code audit) established that the shipped `token-bay-tracker run` binary **cannot complete the headline consumer→seeder→settlement flow**, and several deploy-critical surfaces are unwired. The full list of confirmed gaps is in §3. Per the scope decision, we fix them (test-first) as part of this work, then the e2e suite proves the fixes.

This document is the contract for both workstreams: the **product fixes** (§3) and the **e2e harness** (§4–§8).

---

## 2. Architecture overview

```
                         docker compose network "tokenbay-e2e"
  ┌───────────────────────────────────────────────────────────────────────┐
  │                                                                         │
  │   tracker-a  ◄───────── federation QUIC (ALPN tokenbay-fed/1) ────────► tracker-b
  │   region=A                                                        region=B
  │   :7777 rpc   ▲                                            ▲   :7777 rpc
  │   :7443 fed   │ plugin QUIC (ALPN tokenbay/1)              │   :7443 fed
  │   :9090 admin │                                            │   :9090 admin
  │   :9100 /metrics                                           │   :9100 /metrics
  │       ▲       │                                            │
  │       │       │                                            │
  │   fedactor    │                                            │
  │  (Byzantine   │                                            │
  │   neighbor)   │                                            │
  │               │                                            │
  │        ┌──────┴───────┐                          ┌─────────┴──────┐
  │        │  consumer     │ ── direct tunnel ──────► │   seeder        │
  │        │  actor        │   QUIC (ALPN tb-tun/1)   │   actor         │
  │        │  :8081 ctrl   │   pinned by ephemeral    │   :8082 ctrl    │
  │        └───────────────┘   keys                   └─────────────────┘
  │                                                                         │
  └───────────────────────────────────────────────────────────────────────┘
             ▲ HTTP control + admin assertions
        Go test driver (host / CI runner)  //go:build e2e
```

### 2.1 Module placement (hard constraint)

Go `internal/` visibility forces a multi-module layout:

| Component | Imports | Must live in |
|---|---|---|
| Consumer + seeder actor binary | `plugin/internal/{trackerclient,tunnel,identity,envelopebuilder,exhaustionproofbuilder}` | **plugin module** — `plugin/cmd/tokenbay-e2e-actor/` |
| Federation neighbor (Byzantine) actor | `tracker/internal/federation` | **tracker module** — `tracker/test/e2e/cmd/fedactor/` |
| Real regional trackers | (the product binary) | `tracker/cmd/token-bay-tracker` (existing) |
| Go test driver | admin HTTP + shared types only | **tracker module** — `tracker/test/e2e/` behind `//go:build e2e` |
| Key/config generator | `shared` + stdlib | `tracker/test/e2e/cmd/e2egen/` |

The e2e Go tests carry a `//go:build e2e` tag so `make -C tracker test` (which runs `go test ./...` on the 3-OS CI matrix, including Docker-less macOS/Windows) never compiles or runs them.

### 2.2 Controllable actors: control-plane model

Each actor is a long-lived container that, on boot, generates its Ed25519 identity, enrolls with its assigned tracker, opens the persistent mTLS QUIC connection, and then exposes a small **HTTP control API** the driver uses to make it act deterministically. The actor is otherwise passive.

**Consumer actor control API** (`:8081`):
- `GET /healthz` — ready once enrolled + connected.
- `GET /identity` — `{identity_id_hex, pubkey_hex}`.
- `POST /request` `{model, max_input, max_output}` → performs Balance RPC → builds `ExhaustionProofV1` + signed `EnvelopeSigned` → `BrokerRequest`; on `SeederAssignment`, dials the seeder tunnel, sends a canned `/v1/messages` body, reads the response; returns `{outcome, assignment, response_summary, request_id}`.
- `GET /settlement/last` — last `SettlementPush` handled and the `Settle` result.
- `POST /transfer` `{dest_tracker_id_hex, amount}` → drives the `TransferRequest` RPC against the destination tracker.
- `GET /balance` — Balance RPC for self.

**Seeder actor control API** (`:8082`):
- `GET /healthz`, `GET /identity`.
- `POST /config` `{available, headroom, models[], tier_bitmask, canned_sse_b64}` — sets advertise/serve behavior.
- The actor auto-runs the advertise loop, handles `OfferPush` (accept, generate per-offer ephemeral key, bind tunnel listener), serves the canned SSE over the tunnel, and sends `UsageReport`.
- `GET /offers/last`, `GET /usage/last` — introspection for assertions.

**Fedactor control API** (`:8083`):
- `POST /handshake` `{target_addr, target_pubkey_hex}` — dial + complete dialer handshake.
- `POST /send/root-attestation` `{hour, merkle_root_hex}` — send a (well-formed or conflicting) ROOT_ATTESTATION.
- `POST /send/equivocation-evidence` `{victim_tracker_id_hex, ...}` — frame a peer.
- `POST /send/revocation` `{identity_id_hex, reason}` — gossip a REVOCATION as itself.
- `GET /received` — envelopes the fedactor received back on its stream (for fan-out assertions).

Rationale for HTTP control over env-var/config-only actors: scenarios need *ordered, mid-run* commands (rate-limit now, then assert, then transfer) with cross-actor coordination timing that a static config can't express. HTTP keeps the driver in one Go process making synchronous assertions.

### 2.3 Why real trackers as neighbors *and* a fedactor

- **Real tracker-b** exercises the actual federation code as both dialer and listener, and is required for an honest cross-region transfer (both ends must run real ledger + transfer logic). This is the faithful "neighbor tracker" the request asks for.
- **fedactor** produces Byzantine inputs a well-behaved real tracker never would (conflicting root attestations, fabricated equivocation evidence, malformed frames). Equivocation/depeer and adversarial-revocation scenarios need it.

---

## 3. Product fixes (test-first, each with module-level coverage)

Each fix is developed under TDD in its own module with unit/integration tests **before** the e2e proves it in Docker. Ordered by ascending risk so value lands early. Every citation below was verified against the working tree at commit `56316c2`.

### P1 — Dockerfile builds (blocker, trivial)
**Problem:** `tracker/deployments/docker/Dockerfile:3` uses `golang:1.23-alpine`, which ships `GOTOOLCHAIN=local`; `go.work` requires `go 1.25.0`, so `go work sync` aborts: `go: go.work requires go >= 1.25.0 (running go 1.23.12; GOTOOLCHAIN=local)`. Confirmed empirically (`make -C tracker docker` fails).
**Fix:** Bump base to `golang:1.26-alpine`. Add a `.dockerignore` (`.git`, `docs/`, `.claude/`, `bin/`, `**/*.md`) so `COPY . .` doesn't bust the cache on every doc edit. Add `RUN mkdir -p /data && chown 1000:1000 /data` to the runtime stage so fresh named volumes are uid-1000-writable.
**Test:** the e2e image build itself; a CI build step.

### P2 — Seeders can register over the network (blocker)
**Problem:** No production code path calls `registry.Register` or `UpdateExternalAddr` (only in-process tests do — `tracker/test/integration/helpers_test.go:156`). A connected seeder's `ADVERTISE` therefore hits `registry.Advertise`→`ErrUnknownSeeder` (`registry.go:97`), and the broker's candidate set (`Match`) is always empty → every `BrokerRequest` returns `NoCapacity`. The consumer→seeder assignment path is unreachable in the shipped binary.
**Fix:** Register a seeder into the registry when it connects/enrolls with the seeder role, and populate its network coordinates:
- On plugin QUIC connect for a peer whose enrolled role includes seeder, upsert a `SeederRecord{IdentityID, NetCoords.ExternalAddr = observed remote addr}` with `Available=false` until the first `ADVERTISE`. (Alternative: upsert-on-advertise. Decision: **register-on-connect** so the reflexive address is captured from the live QUIC connection, and `ADVERTISE`/`Heartbeat` become pure updates as designed.)
- Deregister on disconnect.
**Design note — address model:** the broker hands `seeder.NetCoords.ExternalAddr` to the consumer as `SeederAssignment.SeederAddr` (`broker.go:278`). In the e2e Docker network the seeder's reflexive address (as the tracker observes it) is directly dialable by the consumer, so no STUN/TURN hole-punching is exercised in v1. STUN/TURN remain available (handlers exist) but are out of scope for the happy path.
**Tests:** registry-population unit test; a tracker integration test (`test/integration`) that connects a seeder, advertises, and asserts `Match` returns it; broker-assignment integration test.

### P3 — Offer carries the consumer ephemeral pubkey + request_id (blocker for real tunnel binding)
**Problem:** `broker/offer_loop.go` builds `OfferPush` without field 6 (`consumer_ephemeral_pub`) — `EnvelopeBody` has no ephemeral-pub field to source it from — so the real seeder's binding (pin the consumer's ephemeral key on the tunnel listener) is impossible, and the production `seederflow.HandleOffer` rejects every offer lacking it (`plugin/internal/seederflow/offer.go:49`). Separately, `OfferPush` carries no `request_id`, so the seeder can't reference the correct request in its `UsageReport` (it currently must scrape the tip from admin `/stats` and guess).
**Fix (coordinated `shared/` change — updates plugin + tracker in one commit per repo rule 4):**
- Add `consumer_ephemeral_pub` (32 bytes) to the signed `EnvelopeBody` (so the consumer authenticates the ephemeral key it will present on the tunnel). Update `ValidateEnvelopeBody`, regenerate `.pb.go`, refresh golden vectors.
- Broker copies `EnvelopeBody.consumer_ephemeral_pub` and the tracker-generated `request_id` (= reservation token) into `OfferPush`.
- Consumer actor (and real plugin consumerflow) sets `consumer_ephemeral_pub` when building the envelope; seeder uses `OfferPush.consumer_ephemeral_pub` as the tunnel `PeerPin` and echoes `request_id` in `UsageReport`.
**Tests:** shared validate/golden tests; broker offer-loop unit test asserting the fields propagate; plugin seederflow offer test.

### P4 — Settlement happy-path produces a valid ledger entry (correctness)
**Problem:** `settlement.go:189` hard-codes `ConsumerSigMissing:true` in the body the seeder signs (explicit `TODO(broker-followup): T17.5`), so a correctly-signed consumer `Settle` can never yield a bit-clear entry — the participant sigs would be over different bytes. Worse, the seeder/consumer sign an `EntryBody` that includes ledger-sequencing fields (`prev_hash`, `seq`, `timestamp`) they cannot know ahead of the append, forcing a tip-guess race.
**Fix — decouple the participant signature from ledger sequencing (coordinated `shared/` change):**
- Define a canonical **usage-assertion** sub-message and preimage in `shared/signing`: `CanonicalUsageAssertionPreSig({request_id, consumer_id, seeder_id, model, input_tokens, output_tokens, cost_credits})` — no `prev_hash`/`seq`/`timestamp`/`flags`.
- Seeder signs the usage-assertion in `UsageReport`; the `SettlementPush` preimage the consumer counter-signs is the same usage-assertion.
- Tracker verifies both participant sigs over the usage-assertion, then constructs the full `EntryBody` (assigning `prev_hash`/`seq`/`timestamp`), sets consumer-sig presence honestly, and tracker-signs the chain hash over the full body. Consumer-sig-missing is represented by an **empty `consumer_sig`** column (already nullable in `schema_v1.sql`), not a signed flag bit — the `flags` bit0 semantics are retained only as a derived/storage annotation.
- Resolve the consumer pubkey via the existing `identityProxy`→`server.PeerPubkey` (the consumer is mTLS-connected), completing the deferred T17.5.
**Result:** both settlement paths produce a valid entry — happy path (consumer counter-signs → entry with `consumer_sig` present) and dispute path (timeout → entry with empty `consumer_sig`). Balances move in both.
**Tests:** shared signing tests for the new preimage; ledger append tests for both paths; broker settlement unit + integration tests (happy + timeout + tampered-sig-refused).

### P5 — Cross-region transfer works end-to-end (feature completion)
**Problem:** `federation.Deps.Ledger` is never wired in `run_cmd.go:203-219`, so dest-side `StartTransfer` returns `ErrTransferDisabled` and the source drops `TRANSFER_PROOF_REQUEST`; the admin `transfer_reversal` route 400s. Additionally: (a) no `identity_id ↔ consumer_pub` binding on the transfer path (any key can drain any identity); (b) the ledger's `AppendTransferOut` verifies a consumer sig over the `EntryBody` while federation only holds the consumer sig over the canonical `TransferProofRequest` — a pass-through can't satisfy it; (c) dest has no completed-transfer cache, so replaying a nonce double-credits `transfer_in`. The wire types (`TransferRequest`, `TransferProofRequest`, `CanonicalTransferProofRequestPreSig`) and the api-layer sig check already exist (`transfer_request.go`).
**Fix:**
- **Wire `federation.Deps.Ledger`** with a `LedgerHooks` adapter over the real ledger.
- **Reconcile the sig contract:** `AppendTransferOut`/`AppendTransferIn` verify the consumer's authorization as the sig over `CanonicalTransferProofRequestPreSig` (the intent), stored alongside the entry, instead of a sig over the sequencing-dependent `EntryBody`. (Same principle as P4: participant authz is over a sequencing-independent canonical intent.)
- **Bind identity:** the source verifies `sha256(x509.MarshalPKIXPublicKey(ConsumerPub)) == IdentityId` (unifying on the enrollment identity encoding = SHA-256 of DER SPKI) before debiting, and debits *that* identity's on-ledger balance. Closes the "drain any identity" hole.
- **Idempotency:** add the dest-side completed-transfer cache keyed by `ref = nonce` and an on-chain `ref` existence check so replays are no-ops at both ends. Preserve the ordering invariant (source commits `transfer_out` durably before returning the proof).
- **Direction:** consumer sends `TransferRequest` to the **destination** tracker (as the handler already expects); the e2e drives `trackerclient.Client.TransferRequest` directly with correct federation ids (not the broken plugin CLI, which is a documented v1 placeholder and out of scope to fix here beyond a note).
**Tests:** federation transfer-coordinator unit tests (source + dest, idempotent replay, bad-binding rejected); a tracker integration test with two in-process trackers over the in-proc federation transport; then the Docker e2e proves it across two real containers.

### P6 — Serve `/metrics` (deploy-readiness + assertion surface)
**Problem:** `metrics.listen_addr` (default `:9100`) is parsed and validated but **no `promhttp` handler is served anywhere**; all counters register on `prometheus.DefaultRegisterer` unreachable over HTTP.
**Fix:** Start a `promhttp` listener on `metrics.listen_addr` in `run_cmd.go`. This is a genuine deploy-readiness gap (operators expect `/metrics`) and gives the e2e a rich, low-cost assertion surface (broker offer/accept counters, settlement outcomes, federation equivocations/revocations).
**Tests:** a unit/integration test asserting the endpoint serves and exposes a known counter.

### P7 — Admin freeze / unfreeze route (operability + deterministic revocation test)
**Problem:** A real z-score `FROZEN` transition is effectively unreachable from wire traffic (`FROZEN` needs ≥3 breach reasons within 7 days, but the evaluator can append at most one `zscore` reason per OK→AUDIT, and the other breach-ingest paths have no production callers). There is also no operator lever to freeze/revoke a known-bad identity — a deploy-readiness gap for abuse response.
**Fix:** Add authenticated admin routes `POST /identity/{id}/freeze` and `POST /identity/{id}/unfreeze` that drive the reputation store to FROZEN/OK and, on freeze, invoke the existing `OnFreeze`→REVOCATION gossip path. This makes the revocation-propagation scenario deterministic (freeze on A → gossip → B enforces) and gives operators a real control.
**Documented limitation:** automatic z-score→FROZEN remains unreachable in v1; we document it as a known reputation-tuning gap rather than redesigning the state machine in this effort. (Called out explicitly in the readiness report.)
**Tests:** admin route unit tests; reputation transition test; e2e revocation-propagation scenario.

### Product-fix summary

| ID | Change | Module(s) touched | Risk | Wire change? |
|---|---|---|---|---|
| P1 | Dockerfile base + `.dockerignore` + data dir chown | tracker (deploy) | trivial | no |
| P2 | Seeder register-on-connect + addr | tracker | low | no |
| P3 | Offer carries ephemeral pub + request_id | shared+plugin+tracker | medium | **yes** (EnvelopeBody, OfferPush) |
| P4 | Usage-assertion signing; happy-path settlement | shared+plugin+tracker | medium-high | **yes** (new canonical preimage) |
| P5 | Transfer enablement + identity binding + idempotency | tracker (+shared helpers) | high | no (types exist) |
| P6 | Serve `/metrics` | tracker | low | no |
| P7 | Admin freeze/unfreeze + revocation | tracker | low-medium | no |

---

## 4. E2e harness components

1. **`e2egen`** (`tracker/test/e2e/cmd/e2egen/`) — pre-compose key/config generator. Generates raw 64-byte Ed25519 identity keys for tracker-a, tracker-b, and the fedactor; computes federation `tracker_id = sha256(rawpubkey)`; renders `tracker-a.yaml` / `tracker-b.yaml` (with each other in the `federation.peers` allowlist, fedactor allowlisted on tracker-a), all with e2e-tuned timings (§6). Writes to a generated dir mounted into the containers. Idempotent; deterministic given a fixed seed passed as a flag (no `Date.now`/random surprises — seeds are explicit).
2. **`tokenbay-e2e-actor`** (`plugin/cmd/tokenbay-e2e-actor/`) — the consumer/seeder actor (role via flag), control API per §2.2. Static `CGO_ENABLED=0` build.
3. **`fedactor`** (`tracker/test/e2e/cmd/fedactor/`) — Byzantine federation neighbor, control API per §2.2.
4. **Dockerfiles:** reuse the fixed `tracker` image (P1) for the trackers; one shared multi-stage Dockerfile builds `tokenbay-e2e-actor` and `fedactor` (repo-root context, `go work sync`, static builds).
5. **`compose.e2e.yaml`** — the topology in §2, with healthchecks (busybox `wget --header "Authorization: Bearer $TOKEN_BAY_ADMIN_TOKEN"` against `/health`; admin bound to `0.0.0.0:9090`), a named data volume per tracker, `depends_on: service_healthy` ordering, and the shared network.
6. **Go test driver** (`tracker/test/e2e/*_test.go`, `//go:build e2e`) — brings the stack up (via `compose` helper or `docker compose` shell-out), waits for health, runs the scenario suite (§5), tears down. Assertions via admin HTTP client, BALANCE RPC (a thin QUIC client), `docker exec … sqlite3` for ledger tables, `/metrics` scrape, and `docker logs`.
7. **`make -C tracker test-e2e`** target + a dedicated ubuntu-only CI job (§8).

---

## 5. Scenario matrix (the proof)

Each scenario is an independent Go subtest that asserts against the surfaces noted. Grouped; ✔ = provable after the listed fixes land.

**Bring-up & identity**
1. **Health & topology** — all containers healthy; startup ledger-integrity gate logged; tracker-a↔tracker-b reach `Steady` (admin `GET /peers`). ✔ (P1)
2. **Enrollment & starter grant** — consumer + seeder enroll; `GET /identity/{id}` balance == 1000; `GET /stats` tip advances per enroll; `EnrollResponse.starter_grant_entry` verifies (`VerifyEntry`). ✔ (P1)

**Consumer → seeder → settlement (the headline flow)**
3. **Broker assignment** — consumer `POST /request`; broker assigns the seeder; `SeederAssignment{addr, seeder_pubkey, reservation_token}` returned; `GET /broker/inflight/{id}` shows `assigned`. ✔ (P2, P3)
4. **Tunnel data plane** — consumer dials the seeder tunnel pinned by the real ephemeral binding (P3), sends a canned `/v1/messages` body, receives the canned SSE. Wrong-pin dialer is rejected (negative). ✔ (P3)
5. **Settlement happy path** — seeder `UsageReport` → `SettlementPush` → consumer counter-signs `Settle` → USAGE ledger entry with **both** participant sigs; consumer debited, seeder credited (BALANCE RPC + `entries`/`balances` tables). ✔ (P4)
6. **Settlement dispute/timeout** — consumer configured silent → after `settlement_timeout_s` a USAGE entry appends with empty `consumer_sig`; balances still move. ✔ (P4)
7. **Abandoned assignment** — consumer gets an assignment but never uses the tunnel → after the reaper tick the reservation is reclaimed (`/broker/reservations` slot gone, `/broker/inflight` `failed`). ✔ (P2) *(≈35s wait budget — reaper tick is a hardcoded 30s; noted.)*

**Ledger integrity & durability**
8. **Restart integrity** — restart tracker-a; the startup integrity gate passes on the now-non-empty chain (log + `/metrics` counter). ✔
9. **Corruption tripwire (negative)** — mutate a ledger row via `docker exec sqlite3`, restart → boot fails non-zero with an integrity error. ✔
10. **Graceful drain** — `SIGTERM` (or `POST /maintenance`) → tracker drains and exits cleanly; re-open the ledger and assert no corruption / tip intact. ✔

**Federation (real neighbor + Byzantine actor)**
11. **Root-attestation exchange** — tracker-a and tracker-b exchange `ROOT_ATTESTATION`s; both `peer_root_archive` tables gain the counterpart's row. ✔
12. **Equivocation → depeer** — fedactor sends two conflicting `ROOT_ATTESTATION`s for the same `(tracker_id, hour)` → tracker-a detects the conflict, broadcasts `EQUIVOCATION_EVIDENCE`, and depeers the fedactor (`GET /peers` no longer Steady; `/metrics` `equivocations_detected`). ✔
13. **Revocation propagation** — freeze an identity on tracker-a (admin `POST /identity/{id}/freeze`, P7) → `REVOCATION` gossips to tracker-b → a `BrokerRequest` from that identity on tracker-b returns `FROZEN`; `peer_revocations` row present on B. ✔ (P7)

**Cross-region transfer**
14. **Transfer happy path** — consumer enrolled at A transfers N credits toward B; `transfer_out` at A + `transfer_in` at B; balances move by N at both; `TransferProof` returned synchronously. ✔ (P5)
15. **Transfer idempotency** — replay the same nonce → no double credit at either end (dest cache + on-chain `ref` check). ✔ (P5)
16. **Transfer authz (negative)** — a `TransferRequest` whose `ConsumerPub` doesn't match `IdentityId` is rejected (`UNAUTHENTICATED`); a transfer exceeding source balance fails without partial application. ✔ (P5)

**Negative / security / robustness**
17. **Auth & framing** — unauthenticated admin request → 401; oversize QUIC frame (>1 MiB) rejected; TLS 1.2 / non-Ed25519 client cert rejected. ✔
18. **Envelope validation** — tampered consumer sig → `BrokerRequest` rejected; unknown model → `UNKNOWN_MODEL`; stale (>10 min) balance proof → rejected. ✔
19. **Reconnect integrity** — bounce the federation link; on reconnect the reconnect-integrity gate runs and the peer re-attaches (or depeers on local corruption). ✔
20. **`/metrics` exposition** — `:9100/metrics` serves and exposes broker/federation/reputation counters used by the assertions above. ✔ (P6)

---

## 6. Determinism & timing config

The e2e `tracker.yaml` (rendered by `e2egen`) tunes for fast, deterministic runs, respecting all validation co-constraints (`validate.go`):

- `reputation.evaluation_interval_s: 1`, `min_population_for_z_score: 3`, `z_score_threshold: 2.5` (fast reputation cycles).
- `settlement.settlement_timeout_s: 3` **with** `tunnel_setup_ms: 500` (must be `< timeout*1000`) and `reservation_ttl_s: 5` (must be `>= timeout`). All values explicit & positive (zero re-defaults to 900/1200/10000).
- `broker.offer_timeout_ms: 1500` (default; actor responds well within it).
- `federation.publish_cadence_s: 2`, redial base low, gossip rate limits left at defaults (two conflicting attestations fit the burst).
- `admin.listen_addr: 0.0.0.0:9090`, `metrics.listen_addr: 0.0.0.0:9100`, `TOKEN_BAY_ADMIN_TOKEN` set per container.
- Pricing table includes the models the consumer actor requests.

**Known fixed-interval waits** (documented, not configurable): reservation reaper tick = 30s (scenario 7 budgets ~35s). Everything else completes in low single-digit seconds.

**Assertion surfaces** (from the audit): admin HTTP (`/health`, `/stats`, `/peers`, `/identity/{id}`, `/broker/*`, `/federation/*`), BALANCE RPC, `/metrics` (after P6), ledger/reputation SQLite via `docker exec sqlite3` (WAL — read inside the container, not across the host bind mount), and `docker logs` zerolog lines.

---

## 7. Layout summary

```
tracker/
  deployments/docker/Dockerfile         # P1: base bump, .dockerignore, data chown
  test/e2e/
    compose.e2e.yaml
    Dockerfile.actors                   # builds tokenbay-e2e-actor + fedactor
    cmd/
      e2egen/                           # keys + rendered configs
      fedactor/                         # Byzantine federation neighbor
    driver/                             # shared driver helpers (admin client, sqlite, compose)
    scenarios_test.go ...               # //go:build e2e subtests (§5)
  Makefile                              # + test-e2e target
plugin/
  cmd/tokenbay-e2e-actor/               # consumer/seeder actor (control API)
.github/workflows/ci.yml               # + ubuntu-only e2e job
docs/superpowers/specs/
  2026-07-08-tracker-e2e-testing-design.md   # this doc
```

---

## 8. CI & make integration

- **`make -C tracker test-e2e`**: `e2egen` → `docker compose -f test/e2e/compose.e2e.yaml build` → `go test -race -tags=e2e ./test/e2e/...` (the test manages `up`/`down`) → `compose down -v`.
- **CI job** (new, in `ci.yml`): `ubuntu-latest` only (macOS runners lack Docker; Windows runs Windows containers only). Mirrors the existing `integration` job shape: checkout → `setup-go` → `go work sync` → `make -C tracker test-e2e`. Expected ~4–8 min cold (image pulls + module downloads dominate). Does **not** join the 3-OS `test` matrix (the `e2e` build tag keeps it out of `go test ./...`).
- Optional: raise `net.core.rmem_max` on the runner to silence quic-go UDP-buffer warnings (non-fatal for control-plane traffic; low flake risk).

---

## 9. Deliverables

1. The seven product fixes (P1–P7), each test-first with green module tests.
2. The Docker e2e harness (actors, fedactor, generator, compose, driver).
3. The 20-scenario suite, green.
4. `make -C tracker test-e2e` + CI job.
5. A **deploy-readiness report** (`docs/superpowers/…` or PR body) summarizing what the e2e proves, plus documented residual limitations (automatic z-score→FROZEN reputation gap; STUN/TURN hole-punch path not exercised in v1; plugin transfer CLI still a v1 placeholder).

---

## 10. Out of scope (v1)

- STUN/TURN NAT hole-punching under adverse network conditions (Docker gives direct reachability; handlers exist and are smoke-tested, not stress-tested).
- Rewriting the reputation state machine to make automatic z-score freezes reachable (documented as a limitation; P7 gives the operator lever instead).
- Fixing the plugin `transfer_cmd.go` CLI encoding placeholder (the e2e drives `trackerclient.TransferRequest` directly; the CLI fix is noted for a follow-up).
- Load/performance testing against the spec's ≥100-concurrent-connection / latency targets (correctness e2e only; a perf harness is separate work).

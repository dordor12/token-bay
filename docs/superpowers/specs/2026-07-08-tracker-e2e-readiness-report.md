# Tracker Deploy-Readiness Report — Docker E2E

**Date:** 2026-07-11
**Scope:** End-to-end verification of the `tracker` component per the spec `docs/superpowers/specs/2026-07-08-tracker-e2e-testing-design.md` and plan `docs/superpowers/plans/2026-07-08-tracker-e2e-testing.md`.
**Question answered:** *Is the tracker completely working and ready to deploy?*

## Verdict

The tracker's core control-plane is now **validated working end-to-end over a real (Docker-bridge) network**, and the effort **found and fixed four genuine production defects** that were invisible to the pre-existing unit and loopback-integration tests. All **20 e2e scenarios pass** in a single live full-suite run (~102 s) against a two-tracker topology with real consumer, seeder, and Byzantine-neighbor actors.

Cross-region **credit transfer**, which was **entirely disabled** in the shipped binary before this work, now round-trips end-to-end between two real trackers. The **consumer→seeder→settlement** headline flow — previously unreachable black-box because seeders could never enter the registry — works end-to-end.

Deploy-readiness is **materially improved but not unconditional**: several documented limitations remain (below). None blocks the core flows; each is either a known-v1 simplification or a narrow observability/robustness gap with a stated fix direction.

## What the 20 scenarios prove (all green, live)

| # | Scenario | What it validates |
|---|---|---|
| 1 | Health & topology | Both trackers boot healthy; federation peers bidirectionally *steady*; startup integrity gate fires |
| 2 | Enrollment & starter grant | mTLS enroll → SPKI-hash identity + 1000-credit starter grant on the ledger |
| 3 | Broker assignment | Advertised seeder is selected; consumer gets a `seeder_assignment` |
| 4 | Tunnel data plane | Consumer dials the seeder tunnel (port-substituted, ephemeral-pinned) and receives the served bytes |
| 5 | Settlement happy path | Consumer counter-signs the usage-assertion; USAGE ledger entry with both sigs; balances move |
| 6 | Settlement dispute | Consumer stays silent → timeout → `consumer_sig_missing` entry; balances still move |
| 7 | Abandoned assignment | Assignment never dialed → reservation reaped, request `failed` |
| 8 | Restart integrity | Chain survives a tracker restart; startup gate re-verifies |
| 9 | Corruption tripwire | A tampered ledger entry fails the integrity gate (self-restoring) |
| 10 | Graceful drain | SIGTERM → clean exit → restart with intact chain |
| 11 | Root-attestation exchange | Two real trackers gossip + archive each other's Merkle roots |
| 12 | Equivocation → depeer | Byzantine fedactor sending conflicting roots is detected + depeered |
| 13 | Revocation propagation | Admin-freeze on A → signed REVOCATION archived on B |
| 14 | Cross-region transfer | `transfer_out`@A + `transfer_in`@B share the nonce; balances move exactly N |
| 15 | Transfer idempotency | On-chain ref-uniqueness holds on both chains (one entry per ref) |
| 16 | Transfer authz | Over-balance transfer rejected with a reason, no partial application |
| 17 | Auth & framing | Admin endpoints require the bearer token (401 without, 200 with) |
| 18 | Envelope validation | Unpriced model rejected as `UNKNOWN_MODEL`, zero credits spent |
| 19 | Reconnect integrity | Federation bounce → reconnect gate fires → peers re-attach |
| 20 | Metrics exposition | Unauthenticated `/metrics` serves real Prometheus counters |

## Product fixes landed

### Planned deploy-readiness fixes (P1–P7)
- **P1** — Dockerfile builds (`golang:1.26-alpine` base; `/data` writable by uid 1000). *It did not build before.*
- **P2** — Seeders register on connect. *Nothing populated the registry before → every broker request returned `NoCapacity`.*
- **P3** — Offer carries the consumer ephemeral pubkey + `request_id` (coordinated `shared!` change).
- **P4** — Settlement produces a valid ledger entry on **both** the counter-signed and timeout paths (sig-domain rework to a sequencing-independent usage-assertion), on the tracker and in the plugin.
- **P5** — Cross-region transfer wired end-to-end (federation↔ledger hooks + source & dest on-chain ref-uniqueness). *Every transfer returned `ErrTransferDisabled` before.*
- **P6** — `/metrics` served (was defined-but-never-served).
- **P7** — Reputation Freeze/Unfreeze + admin routes (a deterministic operator lever; the z-score path can't reach FROZEN in v1).

### Defects the e2e caught and fixed (invisible to unit/loopback tests)
1. **Heartbeat/RPC stream race** (`plugin` trackerclient). The client signaled *connected* before establishing the heartbeat stream, so under real-network latency an application RPC could steal stream-0 and be consumed by the tracker's heartbeat handler — enroll returned an empty response with a zero identity and no grant, and the connection entered a reconnect loop. Fixed by establishing the heartbeat stream synchronously before signaling connected. **This would have bitten real deployments over real networks.**
2. **Settlement replay / double-spend** (`tracker` broker+ledger). The sequencing-independent settlement authorization was not single-use; a duplicate `usage_report` (even a benign seeder retry) could mint two ledger entries. Fixed with a broker pending-guard + on-chain USAGE `request_id` uniqueness.
3. **Transfer double-credit across a dest restart** (`tracker` ledger). Fixed with durable dest-side `transfer_in` ref-uniqueness.
4. **Integrity-gate tip blind spot** (`tracker` ledger). `AssertChainIntegrity` verified only forward hash-linkage, so a corrupted **tip** entry escaped the startup/reconnect gate. Fixed by re-verifying each entry's body hash against its stored hash, including the tip.
5. **Transfer reject/reversal dead on the wire** (`shared` federation). `ValidateEnvelope` capped kinds at `KIND_PEER_EXCHANGE`, so `KIND_TRANSFER_REJECT`/`KIND_TRANSFER_REVERSAL` were rejected — an insufficient-balance transfer hung the full 30 s timeout and returned a redacted `INTERNAL`. Fixed by widening the bound + surfacing the reject reason as `INVALID`.

## Documented residual limitations (not blockers)

- **Reputation z-score → FROZEN is unreachable in v1** by external traffic; P7's admin Freeze/Unfreeze is the operator lever. Automatic freezing is a follow-up.
- **Transfer reject reason** is now surfaced, but the federation error-string sentinel matching (`isLedgerTransferRefExists`, and the reject-detection path) is exact-string-coupled — a future re-wording would silently degrade behavior (fail-safe: still no double-spend). Prefer typed errors in a follow-up.
- **`trackerclient.callUnary` doesn't honor the caller's context deadline** while awaiting a unary response (an actor's 10 s budget observed a 30 s RPC before the reject fix). General robustness gap worth closing.
- **Merkle-root exchange (scenario 11)** can't be observed in a short e2e run — rollups are hourly and the target bucket is always the previous closed wall-clock hour. The wire gossip + archival path is exercised via SQL-seeded roots; in a real deployment roots are produced hourly. Test-timing artifact, not a product gap.
- **STUN/TURN hole-punch path is not stress-tested** — the Docker bridge gives direct reachability, and the tunnel uses a fixed advertised port; NAT-traversal robustness is out of this suite's scope.
- **Persistent transfer idempotency caches** — the federation in-memory replay caches are unit-test-covered; the durable defense is the on-chain ref-uniqueness (validated live).
- **quic-go UDP buffers** — the CI e2e job raises `net.core.rmem_max`; on hosts where it can't be raised, quic-go logs a one-time warning (control-plane traffic is small; negligible flake risk observed).

## How to run

```
make -C tracker test-e2e     # builds both images, brings up the stack, runs all 20 scenarios
```

Requires Docker + `docker compose`. CI runs it as the ubuntu-only `e2e` job (the `e2e` build tag keeps it out of the 3-OS `go test ./...` matrix). The suite generates its topology deterministically (`e2egen`) and tears the stack down after.

## Bottom line

The tracker's control-plane — enrollment, brokering, tunneling, settlement, ledger integrity, federation, and cross-region transfer — is proven working end-to-end over a real network, and the exercise hardened it against five real defects. With the documented limitations understood and accepted (or scheduled as follow-ups), the tracker is in materially better deploy-readiness shape than when this effort began, when it did not build a Docker image, could not broker a request, and could not transfer credits at all.

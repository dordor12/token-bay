# Federation follow-up roadmap (post-slice-5)

> One-page sequencing for the 12 outstanding federation tasks (#6–#17 in the local task list). Each slice gets a one-paragraph design sketch — enough to start brainstorming the formal spec when the slice is picked up. Not a full plan per slice.

**Authoritative spec:** `docs/superpowers/specs/federation/2026-04-22-federation-design.md`.

**Branch:** `tracker/federation-followup` off `origin/main` at `88bdc97`.

---

## Suggested ordering

The "best next slice" is **#6 plugin reroute**. Slices 4 and 5 wired all the data — a signed bootstrap-peer list with real health scores — but nothing yet consumes it. Until slice 6 lands, the whole chain (bootstrap → ranking → reconnect) is end-to-end useless. After #6, slice 7 (periodic peer-exchange ticker) is a small follow-up that lets gossip actually drive itself.

Slices fall into four tiers:

| Tier | Slices | Theme |
|---|---|---|
| **A — Unlock prior work** | #6, #7, #8 | Close the gaps left by slices 4/5/3 so the federation core actually runs in production cadence |
| **B — Production readiness** | #9, #10, #11 | Gossip safety + table hygiene + automatic depeer |
| **C — Integration backlog** | #12, #13 | Hook the existing wire formats into registry/admission/transfer paths |
| **D — Spec §9 open questions** | #14, #15, #16, #17 | Items the master spec explicitly lists as deferred |

Within a tier, slices are independent and can run in parallel if you bring in a second engineer. Across tiers, the dependency edges are minimal — only #11 actually requires #8 (no point auto-depeering on health if latency is still missing from the score).

---

## Tier A — Unlock prior work

### #6 Plugin reroute logic — **recommended next**

**Why now.** Slices 4 + 5 produce a signed, ranked bootstrap list with real health scores, but `Client.FetchBootstrapPeers` is never called by the plugin and its return value is never persisted. The whole §7.2 promise (plugin starts with 3–5 trackers, fetches more, picks the best) is unrealized.

**Design sketch.**

- New plugin component `plugin/internal/trackerclient/reroute.go` owning a `*RerouteController` that wraps an existing `*Client` and a ranked candidate set.
- On `Client.Start` (after first successful connection), kick off a one-shot `FetchBootstrapPeers` call. Persist the result to a small on-disk file under the plugin's data dir (`bootstrap_peers.json` with `expires_at`).
- Plugin ranks candidates by `health_score DESC`, breaking ties by lowest measured RTT (re-uses slice 8's latency hook if available — gracefully falls back to health_score alone today).
- Reroute is **opt-in for v1**: a new `RerouteEnabled bool` in `Config`. When true and the current tracker drops, the controller picks the highest-ranked alternative from the cached set and `c.Reconnect(endpoint)` (new method that wraps the existing connect path).
- Re-fetch + refresh the cached set on each successful reconnect.
- TTL: respect `bootstrap_peer_list.expires_at`. Stale list → re-fetch on next attempt.

**Non-goals.** Latency-based ranking (slice 8). Pinning the new tracker as the "home" identity registration (admission concern, separate slice).

**Files touched.** `plugin/internal/trackerclient/reroute.go` (new), `plugin/internal/trackerclient/config.go` (`RerouteEnabled`), `plugin/internal/trackerclient/client.go` (Reconnect helper; expose a "drop hook"). No tracker-side changes.

**Estimated commits.** 8–10 (failing test per controller method, reroute trigger, persistence, integration test that uses two fake trackers).

---

### #7 Periodic peer-exchange ticker

**Why now.** Spec §7.1 says hourly. Slice 3 only ships on-demand `Federation.PublishPeerExchange`. Without this, peer-exchange tables never refresh in production — operators would have to cron-call an admin endpoint that doesn't exist either.

**Design sketch.**

- New goroutine in `subsystem.go:NewFederation` driven by `time.NewTicker(cfg.PeerExchangeCadence)`, defaulting to 1h.
- Cancellable via the existing `f.listenCtx`. Stops cleanly on `Close`.
- On tick: `_ = f.PublishPeerExchange(ctx)`. Errors logged at Warn, never fatal.
- New `PeerExchangeCadence time.Duration` on `federation.Config` (default `time.Hour`, configurable in `tracker/internal/config` as `federation.peer_exchange_cadence_s`).
- Range check: ≥60s to avoid storm; ≤24h sanity ceiling.

**Files.** `tracker/internal/federation/subsystem.go`, `config.go`, `tracker/internal/config/{config,apply_defaults,validate}.go`, `testdata/full.yaml`. One integration test that injects a fast cadence and asserts ≥2 emissions in ≤3× cadence.

**Estimated commits.** 5–6.

---

### #8 Peering latency signal (closes §7.3)

**Why now.** §7.3 lists four signals; slice 5 shipped three. Closes the spec section cleanly. Also a soft prerequisite for slice 6's tie-break and for slice 11's quality-based depeer.

**Design sketch.** Two viable approaches; pick during brainstorming.

- **(a)** App-layer ping/pong. New `KIND_PING` envelope with a nonce + send-timestamp. Peer responds with `KIND_PONG` echoing both. RTT recorded by the originator. Cadence: `cfg.PingCadence` (default 30s).
- **(b)** QUIC connection RTT. Surface `quic.Connection.GetStats().RTT` through `transport.Conn` as a new `RTT() time.Duration` method. Loopback returns 0. No new envelopes.

Recommendation: **(b)** for v1. Smaller change set, no new wire kind, no rate-limiting concerns. (a) is the fallback if QUIC stats prove unreliable in practice.

In `PeerHealth`, add `latency map[ids.TrackerID]time.Duration` updated by a new `OnLatencySample(peer, rtt)` method. Score formula gains a `LatencySub = clamp(1 - rtt/cfg.LatencyTarget, 0, 1)` term. Re-balance weights: 0.5 uptime + 0.2 revgoss + 0.3 latency. Validator enforces 3-way sum=1.0.

**Files.** `tracker/internal/federation/transport.go` (interface widening), `transport_quic.go`, `transport_inproc.go`, `health.go` (new field + Score), `config.go` (new HealthConfig knobs), `subsystem.go` (periodic poller that calls `OnLatencySample` per active peer).

**Estimated commits.** 10–12.

---

## Tier B — Production readiness

### #9 Gossip storm control

**Design sketch.** Per-peer token bucket (`golang.org/x/time/rate.Limiter`) keyed by `(peer_id, kind)`. Inbound dispatcher does `if !limiter.Allow() { drop; metric "rate_limited"; return }` before invoking the per-kind handler. Defaults: 100 REVOCATION/min, 10 ROOT_ATTESTATION/min, 1 PEER_EXCHANGE/min, 10 EQUIVOCATION_EVIDENCE/min (rough — tune from spec acceptance criteria §10). All operator-configurable.

**Files.** `tracker/internal/federation/{ratelimit,subsystem,metrics}.go`, config wiring.

---

### #10 Known_peers auto-pruning + auto-peering policy

**Design sketch.** Two related concerns:

- **Prune:** background goroutine that runs every `cfg.KnownPeersPruneInterval` (default 1h) and deletes gossip-sourced rows whose `last_seen` is older than `cfg.KnownPeersMaxAge` (default 7d). Allowlist rows are never pruned.
- **Auto-peer:** strictly opt-in via `cfg.AutoPeerEnabled` (default false). When true, after each peer-exchange merge, the subsystem dials any gossip-sourced row whose `health_score ≥ cfg.AutoPeerHealthThreshold` (default 0.7) and is not already connected. Operator allowlist trust root preserved — auto-peered trackers are tagged in the registry as `Source: "gossip"` and excluded from admin-only operations.

**Files.** Two new tiny components: `auto_pruner.go`, `auto_peerer.go`. SQL: `Store.DeleteStaleKnownPeers(ctx, cutoff)`.

---

### #11 Reputation-driven automatic depeer on low health score

**Depends on #8** (latency signal makes the score meaningful enough to act on).

**Design sketch.** New `HealthWatcher` polls `*PeerHealth` every `cfg.HealthWatchInterval` (default 5m). For each active peer: if `Score(p, now) < cfg.LowHealthThreshold` (default 0.2) for `cfg.LowHealthSustainedWindow` (default 30m), call `f.Depeer(p, ReasonLowHealth)`. Hysteresis prevents flapping. New depeer reason in the enum; new metric counter; admin can override via slice 17's clear-flag if it was a false positive.

---

## Tier C — Integration backlog

### #12 Storage.ListRevocationsForIdentity + registry/admission integration

**Design sketch.** Add `Store.ListRevocationsForIdentity(ctx, identity_id) ([]PeerRevocation, error)`. On enroll, admission queries it; rejects with `ErrRevoked` if any matching revocation exists from the identity's home region. On every active session, registry runs a periodic check (or wakes via a `OnRevocationObserved` callback already exposed by slice 5) and tears down matching sessions. This closes the §6 third bullet: "any connected session for `identity_id` is terminated".

---

### #13 KIND_TRANSFER_REJECT + walk-and-replay

**Design sketch.** New wire kind. Source-side `transfer.OnRequest` emits `KIND_TRANSFER_REJECT{ref, reason}` when `AppendTransferOut` returns `ErrInsufficientCredits` (or `ErrFrozen`). Destination receives, surfaces typed error to `StartTransfer` caller, no longer hits the request timeout. Walk-and-replay handles `ErrTransferRefExists`: when the source has previously processed the same `ref`, it re-fetches the original proof from its ledger and resends instead of dropping.

---

## Tier D — Master spec §9 open questions

### #14 Transfer reversal — 24h escalation workflow

**Sketch.** Background driver scans for `transfer_out` ledger entries with no matching `transfer_in` after 24h. Emits a new `KIND_TRANSFER_REVERSAL{original_ref, evidence}`. Operator workflow + on-chain semantics need spec-level brainstorming before writing the plan.

### #15 Protocol versioning via HELLO feature flags

**Sketch.** Widen the HELLO envelope with a `feature_flags []string` field. Each side records the intersection of advertised flags. Future slices can branch on `f.PeerSupports(peer, "transfer_reject")` etc. Major-version bumps fail the handshake; minor changes are silently tolerated.

### #16 Partial-federation failure alerting

**Sketch.** New gauge `tokenbay_federation_reachable_fraction` = `peers_in_state_steady / len(allowlisted_peers)`. Recording rule + Prometheus alert recipe in `deployments/`. No protocol change.

### #17 Admin API to clear equivocation flag

**Sketch.** New admin RPC `POST /admin/federation/peers/:id/clear_equivocation`. Bearer-token-gated via `TOKEN_BAY_ADMIN_TOKEN`. Calls a new `PeerHealth.ClearEquivocation(peer)` method (removes the sticky flag — also re-enables auto-peering for that ID if slice 10 has shipped). Audit-logged.

---

## How to use this doc

When you pick up a slice:

1. Take the sketch above, fire up `superpowers:brainstorming` with `"slice N: <subject> per the federation follow-up roadmap"`, and let it pin the concrete decisions.
2. Brainstorming produces a spec doc under `docs/superpowers/specs/federation/`.
3. `superpowers:writing-plans` produces the implementation plan.
4. Execute via `superpowers:executing-plans` (or subagent-driven if available).
5. Open PR, watch CI, merge.

The same cadence slices 1–5 used. The roadmap exists to keep the next-slice choice obvious and to remind future engineers what's still owed against the master spec.

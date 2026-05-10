# Slice 5 — Peer Health Score Design

> Federation §7.3. Computes per-peer `health_score` from real observed signals (uptime, revocation-gossip delay, equivocation incidents) and persists the score onto each peer's `known_peers` row at peer-exchange emit time. Replaces the slice-3 placeholder logic in `peerexchange.go` (allowlist+connected→1.0 / disconnected→0.5 / gossip→verbatim).

## 1. Goal

Replace `tracker/internal/federation/peerexchange.go:66-75` with a real per-peer health computation that reflects three of the four signals named in `docs/superpowers/specs/federation/2026-04-22-federation-design.md` §7.3:

- **Uptime** — time since the most recent successful `KIND_ROOT_ATTESTATION` from the peer.
- **Revocation-gossip delay** — average `received_at − signed_at` for revocations whose issuer is the peer itself.
- **Equivocation incidents** — sticky disqualification once detected.

The fourth signal — peering latency — is explicitly deferred to a follow-up slice (see §3 Non-goals).

## 2. Why this slice

Bootstrap-list (slice 4, `tracker/internal/api/bootstrap_peers.go`) sorts peers by `known_peers.health_score`. Today that column carries the slice-3 placeholder, so the ordering produces near-arbitrary results: every connected allowlist row reports 1.0, every disconnected one 0.5, gossip rows pass through whatever upstream said. Plugins fetching the bootstrap list can't usefully rank trackers, and operators have no observable signal that a peer is misbehaving short of an outright equivocation log line.

Slice 5 closes that gap with the smallest viable computation: three real signals, written through to the existing column, no schema change.

## 3. Non-goals

- **No latency signal.** Federation has no application-layer ping/pong frame; QUIC transport-RTT is not surfaced through `transport.Conn`. Adding either is a separate slice.
- **No on-disk persistence of raw signals.** State is in-memory; on tracker restart, signals are recomputed as new events arrive. The persisted `health_score` column carries the score itself, which is fine for short-window staleness.
- **No `known_peers` schema change.** Reuses the existing `health_score` column.
- **No admin API to clear the equivocation flag.** Per `tracker/CLAUDE.md` rule #4 reputation actions are local-scope; the operator has the federation config and can drop a peer entirely if needed.
- **No change to peer-exchange emit cadence.** Still on-demand via `Federation.PublishPeerExchange`; the score is freshened each time emit runs.
- **No change to bootstrap-peers (slice 4).** It already reads `health_score`; this slice only changes what value lives there.
- **No per-peer Prometheus labels.** Aligns with the existing `tracker/internal/federation/metrics.go` cardinality discipline — no labels keyed on `tracker_id`.

## 4. Architecture

A new `*PeerHealth` value lives on the `Federation` struct. It owns three pieces of in-memory state, all guarded by a single `sync.Mutex`:

| Field | Type | Updated by | Read by |
|---|---|---|---|
| `lastRoot` | `map[ids.TrackerID]time.Time` | `OnRootAttestation(peer, now)` | `Score` |
| `revGossipDelays` | `map[ids.TrackerID]*ringBuf16` | `OnRevocation(issuer, signedAt, now)` (only when `issuer == peer` for that peer's own ring) | `Score` |
| `equivocated` | `map[ids.TrackerID]struct{}` (sticky) | `OnEquivocation(peer)` | `Score` |

No background goroutines. Score is computed on demand. Observe* methods are O(1); Score is O(buffer-size) = O(16) under the lock.

### 4.1 Score formula

```
if equivocated[peer]:
    return 0
uptimeSub  = clamp(1 - (now - lastRoot[peer]) / UptimeWindow, 0, 1)   // 0 if never seen
revgossSub = clamp(1 - mean(revGossipDelays[peer]) / RevGossipWindow, 0, 1)  // 1.0 if buffer empty
score      = UptimeWeight * uptimeSub + RevGossipWeight * revgossSub
```

`UptimeWindow=2h`, `RevGossipWindow=600s`, `UptimeWeight=0.7`, `RevGossipWeight=0.3` by default. Weights MUST sum to 1.0 (validator-enforced, with a float epsilon).

### 4.2 Default rationales

- **Empty ring buffer → revgoss=1.0.** No observations is neutral data, not bad behavior. A peer that simply hasn't issued recent revocations is not penalized.
- **Never-seen ROOT_ATTESTATION → uptime=0.** A peer that has never attested cannot rank above one that attested an hour ago. Net consequence: a freshly-allowlisted peer scores `0.7*0 + 0.3*1 = 0.3` until its first ROOT_ATTESTATION lands. The allowlist seed of `0.5` in `subsystem.go:180` gets overwritten on first emit.
- **Sticky equivocation flag.** "Disqualifying" is the spec word. The flag survives until process restart (when the in-memory state is gone anyway). No TTL, no admin-clear path in this slice.

## 5. Wire format

Unchanged. `KnownPeer.health_score` is already a `double` in `shared/federation/federation.proto` and `shared/proto/rpc.proto`. This slice only changes what value populates the field.

## 6. Storage layer change

Add a new method to `tracker/internal/ledger/storage/known_peers.go`:

```go
// UpdateKnownPeerHealth updates only the health_score column for the
// given tracker_id. No-op (returns nil) if the row does not exist.
// Does not touch last_seen, region_hint, or source. Used by federation
// peer-exchange to write back a freshly computed score without the
// "I just observed this peer" semantics of UpsertKnownPeer.
func (s *Store) UpdateKnownPeerHealth(ctx context.Context, trackerID []byte, score float64) error
```

SQL:
```sql
UPDATE known_peers SET health_score = ? WHERE tracker_id = ?
```

The `KnownPeersArchive` interface in `tracker/internal/federation/known_peers_archive.go` is widened with the same method — `*storage.Store` already satisfies it via Go structural typing once the method is added.

## 7. Wiring

Five integration points in existing code, all minimal:

1. **`subsystem.go` constructor** — build `health := NewPeerHealth(cfg.Health, dep.Now)` alongside the existing coordinators; store on `*Federation` as `f.health`.
2. **`subsystem.go:436-443` (ROOT_ATTESTATION receive)** — after `f.dep.Metrics.RootAttestationsReceived("archived")`, call `f.health.OnRootAttestation(peerID, f.dep.Now())`. Only on the success branch — the equivocation-detected branch already returns before we'd want to credit uptime.
3. **`revocation.go:OnIncoming`** — after canonical sig and archive verify succeed, fire a new `OnRevocationObserved(issuer, signedAt, recvAt)` callback in `revocationCoordinatorCfg`. Subsystem wires the callback to `health.OnRevocation`. Keeps the `health` import out of `revocation.go`.
4. **`equivocation.go` (`Equivocator`)** — gains an optional `onFlag func(ids.TrackerID)` field set by a new `NewEquivocator(...)` parameter; both `OnLocalConflict` and `OnIncomingEvidence` fire it after their existing `Depeer` call. The subsystem passes `health.OnEquivocation` as that callback. No new hook is needed at `subsystem.go:498` — the locally-detected-conflict path already routes through `Equivocator.OnLocalConflict` via the slice-3 `apply.RegisterEquivocator` wiring, and that now fans out to `health`. See §10.
5. **`peerexchange.go:EmitNow`** — replace the placeholder block (`peerexchange.go:66-75`) with a `Score()` call followed by `archive.UpdateKnownPeerHealth(...)` write-through, then emit the proto with the fresh score. The `PeerConnected` callback drops out entirely.

`peerexchange.go`'s `peerExchangeCoordinatorCfg` gains a `Health *PeerHealth` field and loses `PeerConnected func(...)`.

## 8. Configuration

`tracker/internal/config/config.go` `FederationConfig` gains a `Health` sub-struct:

```go
type HealthConfig struct {
    UptimeWindow        time.Duration // default 2h
    RevGossipWindow     time.Duration // default 600s
    RevGossipBufferSize int           // default 16
    UptimeWeight        float64       // default 0.7
    RevGossipWeight     float64       // default 0.3
}
```

`tracker/internal/config/validate.go` enforces:
- `UptimeWindow > 0`, `RevGossipWindow > 0`
- `RevGossipBufferSize ∈ [1, 256]`
- `UptimeWeight ∈ [0, 1]`, `RevGossipWeight ∈ [0, 1]`
- `|UptimeWeight + RevGossipWeight − 1.0| < 1e-9` (float-epsilon equality)

Defaults are wired in `DefaultConfig`. Operator can override; values out of range are a startup error.

## 9. Metrics

Add to `tracker/internal/federation/metrics.go`:

- `tokenbay_federation_health_score_computations_total{outcome}` — counter, `outcome ∈ {ok, equivocated, no_data}`.
  - `equivocated` — peer is on the sticky equivocation flag.
  - `no_data` — peer has never sent a ROOT_ATTESTATION AND its ring buffer is empty (so the score is 0.3 by default-fall-through, but operationally the peer is unobserved).
  - `ok` — otherwise.

No per-peer labels (cardinality discipline). Single counter is enough to spot a region where most peers are flagged or unobserved.

## 10. Equivocation flag — both sides

The flag must fire from both:
- **Locally-detected conflict** — `apply.Apply` returns `ErrEquivocation` for a peer's own `KIND_ROOT_ATTESTATION` that conflicts with the archive. The existing path then calls `Equivocator.OnLocalConflict`.
- **Peer-broadcast evidence** — another peer broadcasts `KIND_EQUIVOCATION_EVIDENCE` about a third party; the existing path calls `Equivocator.OnIncomingEvidence`.

Both `Equivocator` methods already compute the offender's `ids.TrackerID` (they call `e.reg.Depeer(offender, ...)` today). The wiring change is local to `equivocation.go`: `Equivocator` gains a new optional `onFlag func(ids.TrackerID)` field, populated via a new parameter on `NewEquivocator`. Both `OnLocalConflict` and `OnIncomingEvidence` fire `onFlag(offender)` after their existing `Depeer` call. The subsystem passes `health.OnEquivocation` as that callback at construction.

The `subsystem.go:498` site (the `RootAttestationsReceived("conflict")` branch) does not need a separate `health.OnEquivocation` call — by the time we reach that branch, `apply.Apply` has already invoked the registered equivocator hook (`apply.RegisterEquivocator(equiv.OnLocalConflict)`, slice-3 wiring), which now fans out to `health` via `onFlag`.

## 11. Concurrency

`PeerHealth`'s single `sync.Mutex` is held across every read and write. The score-compute path is short (constant-bounded ring traversal), so contention is negligible at expected peer counts.

The receive paths in `subsystem.go:recvLoop` are already serialized per peer; concurrent observation across peers is safe by virtue of the mutex.

`peerexchange.EmitNow` may run concurrently with receive paths. The lock prevents torn reads of the maps.

A `-race` test exercises concurrent Observe* + Score; see §12.

## 12. Testing

| Test file | What it covers |
|---|---|
| `tracker/internal/federation/health_test.go` (new) | Unit. Score values at t=0, t=1h, t=2h, t=3h after a ROOT_ATTESTATION. Empty-ring → 1.0, single 0s → 1.0, single 600s → 0.0, averaged samples, capacity wrap-around. Equivocation sticks and overrides max signals. Revgossip ignores revocations whose issuer ≠ peer. Concurrent Observe* + Score under `-race`. |
| `tracker/internal/federation/peerexchange_test.go` (modify) | Existing tests update: the hardcoded 0.5/1.0 expectations become values driven by an injected `*PeerHealth` with synthetic observations. Verify `UpdateKnownPeerHealth` is called per row before the emit. |
| `tracker/internal/federation/integration_test.go` (extend two-peer scenario) | Peer A receives `ROOT_ATTESTATION` at t=0, peer B never does; at t=2h, A's persisted score > B's. Add a revocation issued by A with `signedAt = now-30s`; revgossip subscore drops accordingly. Equivocation detection on B flips B's persisted score to 0 on the next emit. |
| `tracker/internal/ledger/storage/known_peers_test.go` (modify) | New test for `UpdateKnownPeerHealth`: updates existing row, no-op on missing row, no other column touched. |
| `tracker/internal/federation/metrics_test.go` (modify) | Counter labels exercised end-to-end via `Score`. |

## 13. Failure modes

- **`UpdateKnownPeerHealth` returns an error mid-emit.** Logged via `pc.cfg.Invalid("health_persist")` and the score is still emitted on the wire (in-memory `health` value used). The emit does not abort. This matches the slice-3 idiom: a single-row persistence failure should not block the whole gossip cycle.
- **`PeerHealth` map grows unboundedly?** Bounded by the number of distinct `tracker_id`s observed in this process's lifetime — same envelope as the federation registry, which is operator-allowlist-bounded plus gossip-discovered peers (also capped by `peer_exchange` validators in `shared/federation/validate.go`). No GC needed for slice 5.
- **Clock skew / negative deltas.** `now - signedAt` may be negative if a peer signs in the future relative to our wall clock. Treat as zero-delay (best case for the peer); `clamp(...)` handles the corresponding score endpoint. The validator on `Revocation.signed_at` already caps absolute weirdness.

## 14. Migration / rollout

This change is fully internal to the tracker. No schema migration. Plugins observe the difference only as fresher `health_score` values in their bootstrap-peers fetch. A tracker rolled back to slice 4 will simply re-populate `known_peers.health_score` with the slice-3 placeholder on the next peer-exchange emit.

## 15. Future work (out of scope here)

- **Latency signal (§7.3 #2).** Add `KIND_PING` round-trip timing or surface QUIC transport-RTT through `transport.Conn`.
- **Score history / time series.** A small ring of `(t, score)` per peer for forensic dashboards.
- **Admin API to clear equivocation flag.** Once the broader admin API is stood up.
- **Reputation-driven peer dropping.** If a peer's score stays below a threshold for some window, automatically de-peer. Currently de-peer only fires on equivocation directly, which is correct for this slice.

## 16. Open questions answered during brainstorm

| Question | Decision |
|---|---|
| Include latency in v1? | No — defer; no in-band latency channel exists yet. |
| Compute timing? | At peer-exchange emit, write through to `known_peers.health_score`. |
| Aggregation shape? | Equiv-gate + weighted sum (`0.7*uptime + 0.3*revgoss`). |
| Whose revocations contribute to peer X's revgoss? | Only revocations whose `issuer == peer X`. |
| Equivocation TTL? | Sticky for process lifetime — lost on restart, no admin-clear. |
| Where does the type live? | `tracker/internal/federation/health.go`, single file. |

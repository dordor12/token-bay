# Federation — Peer Exchange Subsystem Design

| Field | Value |
|---|---|
| Parent | [Federation Protocol](2026-04-22-federation-design.md), [Federation Core + Root-Attestation](2026-05-09-tracker-internal-federation-core-design.md) |
| Status | Design draft |
| Date | 2026-05-10 |
| Scope | Implements §7.1 of the umbrella federation spec: hourly `PEER_EXCHANGE` gossip carrying a `KnownPeer` table, and the durable `known_peers` archive that backs it. Adds the wire format (`KIND_PEER_EXCHANGE`, `KnownPeer`, `PeerExchange`), an outbound publisher tied to the existing `Config.PublishCadence`, an inbound merge handler, a `KnownPeersArchive` Deps slot, a `known_peers` storage table, and metrics. §7.2 (signed plugin bootstrap distribution) and §7.3 (real `health_score` computation) are sibling slices and out of scope. |

## 1. Purpose

Make `tracker/internal/federation` exchange a known-peers table across the federation graph so peers and (in slice 4) plugins can learn the membership of the network beyond the operator-managed allowlist. After this slice:

- Every hour each tracker emits `PEER_EXCHANGE{peers: [KnownPeer …]}` to its peers via the dedupe-and-forward gossip core.
- Inbound `PEER_EXCHANGE` messages are validated, each entry is upserted into a new `known_peers` table (last-write-wins by `last_seen`, allowlist rows pinned), and the envelope is forwarded onward.
- The umbrella spec §10 acceptance criterion *"Peer-exchange populates the plugin's known-peers table to ≥ 50 trackers within 1 hour of bootstrap in a network of that size"* becomes satisfiable at the **tracker** layer (the plugin distribution side is slice 4's job).

## 2. Non-goals

- Plugin-facing distribution of the known-peers list (umbrella spec §7.2). The signed snapshot, plugin endpoint, and plugin-side merge are slice 4. Slice 3 only guarantees the data is **present and queryable** in storage.
- Real `health_score` computation (umbrella spec §7.3). Slice 3 emits placeholder values: `1.0` for connected allowlisted peers, `0.5` for disconnected allowlisted peers, and forwards-verbatim the received `health_score` for gossip-sourced entries. Slice 5 replaces the placeholder with a real metric (uptime, latency, equivocation incidents).
- Auto-peering with newly-discovered trackers from a `PEER_EXCHANGE` payload. The operator-managed allowlist remains the trust root; gossiped entries are advisory hints, never authoritative. A wider trust directory is the same out-of-scope item slice 2 deferred ("a wider directory of trusted-issuer keys is a follow-up").
- Per-entry cryptographic signature on `KnownPeer`. The envelope's `sender_sig` (slice 0) is the only crypto binding; entries are forwarded as data, identical to how revocations carry only the issuer's `tracker_sig` once. KISS — v1 ships without inner per-entry signing.
- Auto-pruning the `known_peers` table. v1 grows monotonically (operator-driven cleanup). A formal expiry/garbage-collection pass is deferred.
- Discovering peers' `region_hint` from anywhere except the operator allowlist (and gossip echoes). No DNS, no external directory, no GeoIP.

## 3. Position in the system

This subsystem extends `tracker/internal/federation`. It depends on:

- `shared/federation/` — extends the proto with `KIND_PEER_EXCHANGE`, `KnownPeer`, `PeerExchange` (§5).
- `tracker/internal/federation` — existing dispatcher, `Gossip.Forward`, `Dedupe`, `Envelope` machinery, the `Clock`/`Now` injection used by the root-attestation publisher.
- `tracker/internal/ledger/storage` — extended with `known_peers` table (additive `CREATE TABLE IF NOT EXISTS`), `UpsertKnownPeer`, `ListKnownPeers`, `GetKnownPeer`.
- `shared/signing` — `DeterministicMarshal` only via `Envelope.SenderSig` (no new canonical helper for `PeerExchange`).

It is consumed by:

- (Future) slice 4: the plugin-facing signed bootstrap-list endpoint reads `ListKnownPeers(limit, byHealthDesc=true)`.
- (Future) slice 5: real-health computation writes back into `known_peers.health_score` based on observed peer-side metrics.

## 4. Architecture

### 4.1 Module layout

```
shared/federation/
  federation.proto                ← MODIFY: +1 Kind value, +2 messages
  federation.pb.go                ← regenerated
  validate.go / validate_test.go  ← MODIFY: ValidatePeerExchange + envelope ceiling

tracker/internal/federation/
  peerexchange.go / peerexchange_test.go  ← NEW: peerExchangeCoordinator (Emit + Ingest)
  known_peers_archive.go                  ← NEW: KnownPeersArchive iface
  subsystem.go                            ← MODIFY: wire coordinator + publisher + dispatcher case + allowlist seed
  config.go                               ← MODIFY: optional KnownPeers Deps slot
  metrics.go / metrics_test.go            ← MODIFY: peer_exchange_emitted/received + known_peers_size

tracker/internal/ledger/storage/
  known_peers.go / known_peers_test.go    ← NEW: Upsert/List/Get
  schema_v1.sql                           ← MODIFY: +CREATE TABLE known_peers
                                                    (additive; IF NOT EXISTS)

cmd/token-bay-tracker/
  run_cmd.go                              ← MODIFY: pass store as KnownPeers Deps slot
```

One file per area of responsibility, mirroring slice 2's pattern.

### 4.2 New public types

```go
// shared/federation (proto-generated):
type KnownPeer struct {
    TrackerId   []byte  // 32
    Addr        string
    LastSeen    uint64
    RegionHint  string
    HealthScore float64
}
type PeerExchange struct { Peers []*KnownPeer }
```

```go
// tracker/internal/ledger/storage/known_peers.go (new file):
type KnownPeer struct {
    TrackerID   []byte    // 32
    Addr        string
    LastSeen    time.Time
    RegionHint  string
    HealthScore float64
    Source      string    // "allowlist" | "gossip"
}

func (s *Store) UpsertKnownPeer(ctx context.Context, p KnownPeer) error
func (s *Store) GetKnownPeer(ctx context.Context, trackerID []byte) (KnownPeer, bool, error)
func (s *Store) ListKnownPeers(ctx context.Context, limit int, byHealthDesc bool) ([]KnownPeer, error)
```

```go
// tracker/internal/federation/known_peers_archive.go (new file):

// KnownPeersArchive is the slice of *storage.Store federation needs for
// peer-exchange gossip. *storage.Store satisfies this via Go structural
// typing; tests pass a fake.
type KnownPeersArchive interface {
    UpsertKnownPeer(ctx context.Context, p storage.KnownPeer) error
    GetKnownPeer(ctx context.Context, trackerID []byte) (storage.KnownPeer, bool, error)
    ListKnownPeers(ctx context.Context, limit int, byHealthDesc bool) ([]storage.KnownPeer, error)
}
```

The federation `Deps` gains:

```go
type Deps struct {
    // existing fields ...
    KnownPeers KnownPeersArchive  // optional; nil disables peer-exchange
}
```

If `dep.KnownPeers == nil`, `Federation.PublishPeerExchange(ctx)` returns `ErrPeerExchangeDisabled` without doing anything, and inbound `KIND_PEER_EXCHANGE` is rejected with `invalid_frames{reason="peer_exchange_disabled"}`. Slice-0/-1/-2 deployments without the archive continue to work; the federation subsystem stays open and only loses peer-exchange gossip.

### 4.3 `peerExchangeCoordinator`

```go
type peerExchangeCoordinator struct {
    cfg peerExchangeCoordinatorCfg
}

type peerExchangeCoordinatorCfg struct {
    MyTrackerID  ids.TrackerID
    MyPriv       ed25519.PrivateKey
    Archive      KnownPeersArchive
    Forward      func(ctx context.Context, kind fed.Kind, payload []byte)
    PeerConnected func(ids.TrackerID) bool   // for placeholder health: 1.0 if true, 0.5 if false (allowlist rows only)
    Now          func() time.Time
    EmitCap      int                          // default 256
    Invalid      func(reason string)
    OnEmit       func()                       // metric: peer_exchange_emitted_total
    OnReceived   func(outcome string)         // outcomes: "merged" | "peer_exchange_disabled"
}

func newPeerExchangeCoordinator(cfg peerExchangeCoordinatorCfg) *peerExchangeCoordinator
func (pc *peerExchangeCoordinator) EmitNow(ctx context.Context) error
func (pc *peerExchangeCoordinator) OnIncoming(ctx context.Context, env *fed.Envelope, msg *fed.PeerExchange)
```

`EmitNow` lists the top-`EmitCap` known peers (sorted by `health_score` descending) from the archive, refreshes the `health_score` for `source='allowlist'` rows using `PeerConnected`, packs them into a `PeerExchange` proto, marshals, and calls `Forward` (which broadcasts to all active peers).

`OnIncoming` validates the message (already done at the dispatcher boundary by `ValidatePeerExchange`) and upserts each entry. Storage's `Upsert` enforces last-write-wins by `last_seen` and refuses to mutate `source='allowlist'` rows from a `gossip` write — both protections are in SQL, not application code, so they survive future callers.

The coordinator does **not** spawn its own goroutine. Outbound emission is driven on-demand by an exposed subsystem method, `Federation.PublishPeerExchange(ctx) error`, that mirrors slice 0's `Federation.PublishHour(ctx, hour)`. Production cadence (a 1h cron driver, an admin endpoint, or a future ticker driver in `tracker/cmd/`) is an operator-layer concern out of scope for this slice; tests call `PublishPeerExchange` directly. Inbound is dispatched on the per-peer recv goroutine.

### 4.4 Concurrency model

- One synchronous emit per call to `Federation.PublishPeerExchange(ctx)`. No background goroutine is spawned by the coordinator or the subsystem.
- Inbound peer-exchange dispatches on the per-peer recv goroutine, same as slice 0's `ROOT_ATTESTATION` and slice 2's `REVOCATION`.
- `KnownPeersArchive` calls happen inside the recv goroutine; storage is SQLite-serialized so contention is bounded.
- All paths must pass `go test -race` (federation is on the always-`-race` list).

## 5. Wire format additions

### 5.1 Kind enum value

```proto
KIND_PEER_EXCHANGE = 13;
```

(slot 12 was taken by slice 2's `KIND_REVOCATION`.)

### 5.2 Messages

```proto
message KnownPeer {
  bytes  tracker_id   = 1;  // 32 bytes
  string addr         = 2;  // e.g. "wss://tracker.example.org:443"; len ≤ 256
  uint64 last_seen    = 3;  // unix seconds; 0 = never
  string region_hint  = 4;  // human-friendly; len ≤ 64
  double health_score = 5;  // 0..1 (clamped on validate)
}

message PeerExchange {
  repeated KnownPeer peers = 1;  // ≤ 1024 entries (validator)
}
```

There is no `tracker_sig` field on `KnownPeer` and no inner-message signature on `PeerExchange`. The envelope's `sender_sig` (slice 0) is the only cryptographic binding. Entries are advisory hints; the receiver does not authenticate any field of any `KnownPeer` against a separate trust anchor in v1.

### 5.3 Validator (`shared/federation/validate.go`)

`ValidatePeerExchange` enforces:

- `len(peers) ≤ 1024`. Otherwise: `too_many_entries`.
- For each entry:
  - `len(tracker_id) == 32` and not all-zero. Otherwise: `bad_tracker_id`.
  - `addr` non-empty, `len(addr) ≤ 256`, valid UTF-8. Otherwise: `bad_addr`.
  - `len(region_hint) ≤ 64`, valid UTF-8. Otherwise: `bad_region_hint`.
  - `0.0 ≤ health_score ≤ 1.0` (NaN rejected). Otherwise: `bad_health_score`.
  - `last_seen` is unconstrained as a value but must fit in `int64` semantics on storage (overflow on the storage write returns the same `bad_last_seen` route).

`ValidateEnvelope`'s ceiling lifts to `KIND_PEER_EXCHANGE`.

### 5.4 Canonical signing

None. There is no `signing_peer_exchange.go`. The envelope-level signing pattern (slice 0) is the only crypto path for this kind.

## 6. `KnownPeersArchive` interface

(See §4.2 for the type definition.) `*ledger/storage.Store` satisfies it via Go structural typing; tests pass a fake.

If `dep.KnownPeers == nil`, the coordinator is not constructed; `Federation.PublishPeerExchange(ctx)` returns `ErrPeerExchangeDisabled`, and inbound `KIND_PEER_EXCHANGE` is rejected with a metric. Slice-0/-1/-2 deployments without the archive continue to work.

## 7. Storage table

`tracker/internal/ledger/storage/schema_v1.sql` additive:

```sql
CREATE TABLE IF NOT EXISTS known_peers (
    tracker_id   BLOB    NOT NULL PRIMARY KEY,
    addr         TEXT    NOT NULL,
    last_seen    INTEGER NOT NULL,           -- unix seconds
    region_hint  TEXT    NOT NULL DEFAULT '',
    health_score REAL    NOT NULL DEFAULT 0.0,
    source       TEXT    NOT NULL CHECK (source IN ('allowlist','gossip'))
);

CREATE INDEX IF NOT EXISTS idx_known_peers_health
    ON known_peers (health_score DESC);
```

Per the ledger spec rule *"Migrations are append-only: a v2 schema lands as a separate set of statements, never as ALTER on existing v1 tables"*, this is additive — new table + index only, no modification of existing slice-0/-1/-2 tables.

`Store.UpsertKnownPeer` issues:

```sql
INSERT INTO known_peers (tracker_id, addr, last_seen, region_hint, health_score, source)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(tracker_id) DO UPDATE SET
    addr         = CASE WHEN known_peers.source = 'allowlist' AND excluded.source = 'gossip'
                        THEN known_peers.addr ELSE excluded.addr END,
    last_seen    = MAX(known_peers.last_seen, excluded.last_seen),
    region_hint  = CASE WHEN known_peers.source = 'allowlist' AND excluded.source = 'gossip'
                        THEN known_peers.region_hint ELSE excluded.region_hint END,
    health_score = CASE WHEN excluded.last_seen >= known_peers.last_seen
                        THEN excluded.health_score ELSE known_peers.health_score END,
    source       = known_peers.source  -- pinned: gossip never demotes/promotes source
WHERE excluded.last_seen >= known_peers.last_seen
   OR known_peers.source  = 'allowlist';
```

Two invariants enforced in SQL:

1. **Last-write-wins by `last_seen`.** A gossip event with stale `last_seen` cannot rewind a fresher entry.
2. **Allowlist rows are pinned.** A `gossip` write cannot mutate the `addr` or `region_hint` of an `allowlist` row, and never changes the `source` field. (`health_score` is allowed to update for allowlist rows so slice-5 metrics can flow in. v1's `EmitNow` refreshes allowlist `health_score` itself before emitting.)

`Store.ListKnownPeers(limit, byHealthDesc)` returns up to `limit` rows. When `byHealthDesc=true`, ordering is `health_score DESC, tracker_id ASC` (deterministic tiebreak). When false, ordering is `tracker_id ASC`.

`Store.GetKnownPeer(trackerID)` reads by primary key.

`storage.KnownPeer` mirrors the SQL columns (see §4.2 type definition).

## 8. Outbound flow

```
caller invokes Federation.PublishPeerExchange(ctx):
  → federation.Federation.peerExchange.EmitNow(ctx):
    1. require dep.KnownPeers != nil (else PublishPeerExchange returns ErrPeerExchangeDisabled before reaching here).
    2. peers, _ := Archive.ListKnownPeers(ctx, EmitCap, byHealthDesc=true).
    3. for each row with source=='allowlist': overwrite health_score with
       PeerConnected(row.TrackerID) ? 1.0 : 0.5. (Placeholder for slice 5.)
    4. build PeerExchange{Peers: [...]} proto.
    5. payload = proto.Marshal(pex).
    6. dedupe.Mark(sha256(payload)).
    7. forward(KIND_PEER_EXCHANGE, payload). The slice-0 forward closure
       broadcasts to all active peers; the dedupe core suppresses echoes.
    8. metric: peer_exchange_emitted_total.
```

Note: `EmitNow` does **not** persist its own emission back into `known_peers`; the local view is already authoritative. Self-entries are not added to the emitted snapshot — peers learn the sender from the envelope, not from the payload.

### 8.1 Allowlist seeding

At `subsystem.Open`, after `Deps.KnownPeers` is verified non-nil, iterate `Config.Peers` and for each `AllowlistedPeer` issue:

```go
Archive.UpsertKnownPeer(ctx, storage.KnownPeer{
    TrackerID:   p.TrackerID[:],
    Addr:        p.Addr,
    LastSeen:    Now(),                     // bootstrap timestamp
    RegionHint:  p.Region,
    HealthScore: 0.5,                       // placeholder; refreshed on tick
    Source:      "allowlist",
})
```

This is idempotent across restarts: the SQL clause leaves `addr` and `region_hint` pinned for allowlist rows, so a re-seed at startup with the same operator config is a no-op. An operator-driven `addr` change in the YAML config takes effect because both rows are `source='allowlist'` (the pinning rule guards `gossip→allowlist` overwrites only).

## 9. Inbound flow

```
recv KIND_PEER_EXCHANGE from peer:
  ─── envelope-sig verified by dispatcher; dedupe.Seen-checked
  │
  ▼
peerExchangeCoordinator.OnIncoming(ctx, env, msg):
  1. require dep.KnownPeers != nil. Else metric peer_exchange_disabled
     (counted on the receiving side, not emitter side).
  2. for each entry in msg.Peers:
       if entry.tracker_id == MyTrackerID: skip (don't archive self).
       Archive.UpsertKnownPeer(ctx, storage.KnownPeer{
           TrackerID:   entry.tracker_id,
           Addr:        entry.addr,
           LastSeen:    time.Unix(int64(entry.last_seen), 0),
           RegionHint:  entry.region_hint,
           HealthScore: entry.health_score,
           Source:      "gossip",
       })
       (storage's SQL handles last-write-wins + allowlist pinning.)
  3. forward(KIND_PEER_EXCHANGE, env.Payload) — the slice-0 forward
     closure dedupes + broadcasts to peers other than fromPeer.
  4. metric: peer_exchange_received_total{outcome="merged"}.
```

`Validate*` already ran at the dispatcher boundary; entries reaching `OnIncoming` are well-formed by construction.

## 10. Failure handling

| Failure | Behavior |
|---|---|
| `dep.KnownPeers == nil` deployment | Publisher goroutine not started; inbound KIND_PEER_EXCHANGE rejected with `invalid_frames{reason="peer_exchange_disabled"}` and `peer_exchange_received_total{outcome="peer_exchange_disabled"}`. |
| Bad shape on `PeerExchange` (caps, fields) | Drop entire envelope at the dispatcher's validator boundary, `invalid_frames{reason=…}`. |
| Storage `UpsertKnownPeer` returns non-nil for one entry | Log + metric. Continue with remaining entries (one bad row should not poison the batch). The envelope is forwarded after the whole loop runs, regardless of per-entry failures (best-effort gossip; other peers may have cleaner storage). |
| Self-entry inside a received `PeerExchange` (issuer == MyTrackerID) | Skip, do not archive (already covered above). |
| Duplicate `Upsert` (same `last_seen`) | SQL clause is `>=`, so a same-second row from a duplicate emit is a no-op write. The dedupe core has already caught the message-level echo at the dispatcher's recv path. |
| `PublishPeerExchange` called while `Archive` is briefly unavailable (rotation, etc.) | `EmitNow` returns the `Archive`'s error verbatim; `PublishPeerExchange` returns the same error to the caller. Caller policy is its own (retry, log, metric). |

## 11. Configuration

No new config fields in `FederationConfig`. `Config.PublishCadence` is consumed by future operator-layer cron drivers (see §4.4); v1 ships the on-demand method only. `Config.SendQueueDepth`, `DedupeTTL`, `DedupeCap` carry over from slice 0.

A new internal constant `defaultPeerExchangeEmitCap = 256` governs the per-emit row cap. Lifting this to a `Config` knob is deferred until an operator asks; v1 hardcodes.

## 12. Metrics

New Prometheus counters/gauges under `tokenbay_federation_`:

- `peer_exchange_emitted_total` — local emit ticks fired (publisher).
- `peer_exchange_received_total{outcome=merged|peer_exchange_disabled}` — inbound dispatcher outcomes.
- `known_peers_size` — gauge, sampled after each emit and once at `Open`. Optional v1; landed in this slice because slice 4 needs it for the bootstrap-list audit.

Existing `invalid_frames_total{reason}` gains `too_many_entries`, `bad_addr`, `bad_region_hint`, `bad_health_score`, `bad_tracker_id`, `bad_last_seen`, `peer_exchange_disabled`.

## 13. Testing

TDD throughout, one conventional commit per red-green cycle.

1. **Wire format.** `ValidatePeerExchange` happy path + bad-shape table (caps, bad UTF-8, NaN, oversized fields).
2. **Storage layer.**
   - `UpsertKnownPeer` happy path: insert, then update with newer `last_seen` succeeds.
   - Stale `last_seen` is a no-op (last-write-wins).
   - Allowlist pinning: a `source='gossip'` upsert against a `source='allowlist'` row leaves `addr` + `region_hint` unchanged.
   - `health_score` updates for allowlist rows when `last_seen` is newer (slice-5 forward path).
   - `ListKnownPeers(byHealthDesc=true)` returns rows sorted by health desc with deterministic tracker_id tiebreak.
   - `GetKnownPeer` for missing returns `ok=false`.
3. **Unit `peerExchangeCoordinator.EmitNow`.** Fake archive with 3 rows (2 allowlist + 1 gossip). Stub `forward` captures payload. Assert: payload has 3 entries, allowlist rows' `health_score` reflects `PeerConnected` (1.0 / 0.5), gossip row passes verbatim, ordering matches archive's `ListKnownPeers(byHealthDesc=true)`.
4. **Unit `peerExchangeCoordinator.OnIncoming`.** Stub archive captures upserts. Happy path: every entry is upserted with `Source="gossip"`; envelope-level forward is invoked.
5. **OnIncoming self-entry skip.** A `PeerExchange` containing an entry with `tracker_id == MyTrackerID` does not archive that entry.
6. **OnIncoming nil-archive rejection.** When `dep.KnownPeers == nil`, dispatcher path bumps `peer_exchange_disabled` and does not forward.
7. **Reputation/freeze isolation.** No coupling to slice 2; the existing `revocations_*` metrics + tests are unchanged. (Smoke test by re-running slice 2's integration suite.)
8. **Subsystem on-demand emit.** Construct a `*Federation` with a fake `KnownPeersArchive`. Call `Federation.PublishPeerExchange(ctx)`; assert `peer_exchange_emitted_total` incremented and the gossip layer received a `KIND_PEER_EXCHANGE` envelope. With `dep.KnownPeers == nil`, assert `ErrPeerExchangeDisabled` is returned.
9. **Two-tracker integration (in-process transport).** A's allowlist contains B and a third tracker T (no live peering with T; just an allowlist row). Test calls `aFed.PublishPeerExchange(ctx)`. B's `known_peers` table then contains a row for T sourced `gossip`. B's row for A remains `source='allowlist'` and its `addr` is unchanged (allowlist pinning verified end-to-end).
10. **Three-tracker line-graph forwarding.** A↔B↔C with no direct A-C link. A's allowlist contains B and a fourth tracker D. Test calls `aFed.PublishPeerExchange(ctx)` and waits for forwarding. C's `known_peers` table contains a row for D (proves forwarding through B). Re-emission from C back through B is dedupe'd by the slice-0 dedupe core.
11. **Race-cleanliness.** All tests pass `-race`.

## 14. Open questions

- **Auto-pruning of stale `known_peers` rows.** Spec doesn't define one. v1 grows monotonically; pruning is operator-driven. A formal expiry pass is deferred.
- **Tunable `EmitCap`.** Hardcoded 256 in v1. If operator feedback says "we need top-1024 for our region," lifting to `Config.PeerExchangeEmitCap` is trivial.
- **Inner-entry signing.** v1 ships without per-`KnownPeer` signatures (advisory hints). If a future malicious-gossip incident motivates per-entry binding, `KnownPeer.tracker_sig = Ed25519(canonical(addr || last_seen || region_hint))` is the natural extension. Not designed for here.
- **Source-of-truth ambiguity for `health_score` on allowlist rows.** v1: `EmitNow` refreshes it from `PeerConnected` at tick time, and the storage write back from a gossip echo cannot regress it. Slice 5 will own this field's update path; v1 tolerates the mild duplication.
- **`region_hint` cross-region disagreement.** Two trackers may emit different `region_hint` strings for the same third tracker (operator typo, stale config). v1: last-write-wins by `last_seen` between gossip rows; allowlist rows are pinned and immutable from gossip. Operator reconciles via config.
- **Shared `Config.PublishCadence` between root-attestation and peer-exchange publishers.** v1 reuses the slice-0 knob; both fire on the same 1h ticker. If operators need to diverge them (e.g., 1h root-attestation, 30m peer-exchange), splitting into `Config.RootAttestCadence` + `Config.PeerExchangeCadence` is mechanical. Not designed for here.

## 15. Acceptance criteria

- Two `Federation` instances peered on the in-process transport. A's allowlist contains B and a non-live entry T. After one `aFed.PublishPeerExchange(ctx)` call, B's `known_peers` table contains a `source='gossip'` row for T.
- A three-tracker line A↔B↔C: A's allowlist contains B and D; after ticks propagate, C archives a row for D and the dedupe core suppresses the round-trip echo.
- Allowlist-row pinning: a `gossip` upsert against B's `source='allowlist'` row for A leaves `addr` and `region_hint` unchanged.
- Last-write-wins by `last_seen`: a stale-`last_seen` upsert is a no-op.
- A `dep.KnownPeers == nil` deployment of slice 0/1/2 is unaffected: existing tests still pass, `Federation.PublishPeerExchange(ctx)` returns `ErrPeerExchangeDisabled`, inbound `KIND_PEER_EXCHANGE` is rejected with `peer_exchange_disabled`.
- `make check` (test + lint) green at repo root, `tracker/`, and `shared/`.
- Federation package continues to pass `go test -race ./...`.

## 16. Subsystem implementation index

- **`tracker/internal/federation` (peer exchange)** — plan: `docs/superpowers/plans/2026-05-10-federation-peer-exchange.md` (to be written via the writing-plans skill).

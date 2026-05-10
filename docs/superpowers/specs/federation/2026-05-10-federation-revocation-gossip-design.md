# Federation — Revocation Gossip Subsystem Design

| Field | Value |
|---|---|
| Parent | [Federation Protocol](2026-04-22-federation-design.md), [Federation Core + Root-Attestation](2026-05-09-tracker-internal-federation-core-design.md) |
| Status | Design draft |
| Date | 2026-05-10 |
| Scope | Implements §6 of the umbrella federation spec: the `REVOCATION` message that propagates identity-freeze decisions across the federation graph. Adds the federation-level wire format, an outbound emit path triggered by the reputation subsystem's freeze events, an inbound handler that persists evidence and forwards via gossip, a small `peer_revocations` storage table, and a minimal `FreezeListener` Deps slot on the reputation subsystem so federation can subscribe without violating the leaf-module rule. |

## 1. Purpose

Make `tracker/internal/federation` carry identity revocations across regions. After this slice:

- When the local reputation subsystem freezes an identity (per the reputation spec §4 evaluator), federation emits `REVOCATION{tracker_id, identity_id, reason, revoked_at, tracker_sig}` to all peers via the dedupe-and-forward gossip core.
- Inbound `REVOCATION` messages are validated, persisted to the new `peer_revocations` table, and forwarded onward.
- The umbrella spec §10 acceptance criterion *"Revocations propagate to all reachable trackers within 5 minutes"* becomes satisfiable at the federation layer.

## 2. Non-goals

- Local **enforcement** of revocations beyond the new archive: terminating active seeder sessions, refusing fresh `enroll` for the revoked identity, and gating broker decisions are done in `tracker/internal/registry` and `tracker/internal/admission` follow-ups. The umbrella spec §6 says the revocation receiver "applies the revocation to its registry"; that wiring is a sibling slice.
- Cryptographic verification of received `REVOCATION` evidence against an authoritative-pubkey directory. v1 trusts the envelope's sender signature and the issuing tracker's pubkey from the operator-managed allowlist (the same trust model as `ROOT_ATTESTATION` in slice 0). A wider directory of trusted-issuer keys is a follow-up.
- Per-region operator-override of advisory revocations (umbrella spec §6.1: "peer trackers honor them by default but can operator-override"). Slice 2 honors all by default; the override knob ships with the operator-admin slice.
- Auto-re-broadcast on receiving a duplicate revocation. The dedupe core handles it for free; no extra work.
- Reputation-internals refactor. Slice 2 adds **one** new optional field on `reputation.Deps` (`FreezeListener`) and **one** call site at the freeze emit point. Anything more invasive is deferred.

## 3. Position in the system

This subsystem extends `tracker/internal/federation`. It depends on:

- `shared/federation/` — extends the proto with `KIND_REVOCATION`, `Revocation` message, `RevocationReason` enum (§5).
- `tracker/internal/federation` — existing dispatcher, `Gossip.Forward`, `Dedupe`, `Envelope` machinery from slice 0.
- `tracker/internal/ledger/storage` — extended with `peer_revocations` table (idempotent `CREATE TABLE IF NOT EXISTS`, additive to v1 schema), `PutPeerRevocation`, `GetPeerRevocation`, `ListRevocationsForIdentity`.
- `tracker/internal/reputation` — extended with one new optional `Deps.FreezeListener` interface and one call to `listener.OnFreeze(id, reason)` at the existing freeze emit point (`evaluator.go:178` on main as of merge of #27).
- `shared/signing` — `DeterministicMarshal` for canonical bytes; new `CanonicalRevocationPreSig` helper.

It is consumed by:

- The reputation subsystem at startup time (federation is constructed first; the `*Federation` is registered as the `FreezeListener` on `reputation.Open`).
- (Future) `tracker/internal/registry` and `tracker/internal/admission` reading the archive to enforce.

## 4. Architecture

### 4.1 Module layout

```
shared/federation/
  federation.proto                ← MODIFY: +1 Kind value, +1 message, +1 enum
  federation.pb.go                ← regenerated
  validate.go / validate_test.go  ← MODIFY: ValidateRevocation
  signing_revocation.go           ← NEW: CanonicalRevocationPreSig
  signing_revocation_test.go

tracker/internal/federation/
  revocation.go / revocation_test.go  ← NEW: RevocationCoordinator (emit + handle)
  revocation_archive.go               ← NEW: PeerRevocationArchive iface (small slice of storage)
  subsystem.go                        ← MODIFY: wire RevocationCoordinator + FreezeListener impl
  config.go                           ← MODIFY: optional RevocationArchive Deps slot

tracker/internal/ledger/storage/
  peer_revocations.go / peer_revocations_test.go  ← NEW: Put/Get/List
  schema_v1.sql                                   ← MODIFY: +CREATE TABLE peer_revocations
                                                            (additive; IF NOT EXISTS)

tracker/internal/reputation/
  reputation.go / reputation_test.go  ← MODIFY: +Deps.FreezeListener; emit at freeze site
  doc.go                              ← MODIFY: note federation hook

cmd/token-bay-tracker/
  run_cmd.go                          ← MODIFY: register federation as reputation FreezeListener
```

One file per area of responsibility, mirroring the slice-1 pattern.

### 4.2 New public types

`FreezeListener` is defined **once** in `tracker/internal/reputation` (the producer side). Federation satisfies it implicitly via Go's structural typing — no `tracker/internal/federation → tracker/internal/reputation` import is needed, and reputation stays a leaf module per its CLAUDE.md rule.

```go
// In tracker/internal/reputation/listeners.go (new file):

// FreezeListener is the reputation→outside notification hook. The
// evaluator calls OnFreeze synchronously when an identity transitions
// to FROZEN. Implementations MUST NOT block — federation's impl just
// signs + enqueues the gossip and returns.
type FreezeListener interface {
    OnFreeze(identityID ids.IdentityID, reason string, revokedAt time.Time)
}

// New optional field on existing Deps:

type Deps struct {
    // existing fields ...
    FreezeListener FreezeListener  // optional; nil disables federation gossip emit
}
```

`*Federation` grows an `OnFreeze` method matching this signature (delegated to its `revocationCoordinator`); `cmd/token-bay-tracker/run_cmd.go` constructs federation first, then passes `federationInst` as `reputation.Deps.FreezeListener` at `reputation.Open` time.

### 4.3 `RevocationCoordinator`

```go
type revocationCoordinator struct {
    cfg revocationCoordinatorCfg
}

type revocationCoordinatorCfg struct {
    MyTrackerID ids.TrackerID
    MyPriv      ed25519.PrivateKey
    Archive     PeerRevocationArchive
    Forward     func(ctx context.Context, kind fed.Kind, payload []byte)
    PeerPubKey  func(ids.TrackerID) (ed25519.PublicKey, bool)
    Now         func() time.Time
    Metrics     *Metrics
}

func newRevocationCoordinator(cfg revocationCoordinatorCfg) *revocationCoordinator
func (rc *revocationCoordinator) OnFreeze(id ids.IdentityID, reason string, revokedAt time.Time)
func (rc *revocationCoordinator) OnIncoming(ctx context.Context, env *fed.Envelope, fromPeer ids.TrackerID)
```

`OnFreeze` synthesizes a signed `Revocation` proto, marks it in dedupe, and calls `Forward` (which broadcasts to all active peers). `OnIncoming` parses + validates + verifies the issuer's signature, persists to the archive, and forwards onward — the slice-0 dedupe core suppresses re-emission of seen messages.

The coordinator does **not** spawn a goroutine; `OnFreeze` returns once the signed envelope is on the per-peer send queues (back-pressure point is the existing `SendQueueDepth`). On the rare event a queue is full, the dropped frame is metric'd and the next gossip wins eventual delivery.

### 4.4 Concurrency model

- One synchronous call per freeze (driven by reputation's evaluator goroutine).
- Inbound revocations dispatch on the per-peer recv goroutine, same as slice 0's `ROOT_ATTESTATION`.
- `PeerRevocationArchive` calls happen inside the recv goroutine; storage is SQLite-serialized so contention is bounded.
- All paths must pass `go test -race` (federation is on the always-`-race` list).

## 5. Wire format additions

### 5.1 Kind enum value

```proto
KIND_REVOCATION = 12;
```

(slots 9–11 are taken by the slice-1 transfer kinds.)

### 5.2 Message + reason enum

```proto
enum RevocationReason {
  REVOCATION_REASON_UNSPECIFIED = 0;
  REVOCATION_REASON_ABUSE       = 1;  // reputation-driven freeze
  REVOCATION_REASON_MANUAL      = 2;  // operator-issued
  REVOCATION_REASON_EXPIRED     = 3;  // identity TTL elapsed
}

message Revocation {
  bytes            tracker_id  = 1;  // 32 bytes — issuer
  bytes            identity_id = 2;  // 32 bytes — revoked identity
  RevocationReason reason      = 3;
  uint64           revoked_at  = 4;  // unix seconds
  bytes            tracker_sig = 5;  // 64 bytes — Ed25519(canonical) by issuer
}
```

`tracker_id` MUST equal the envelope's `sender_id` at the **issuer's** initial broadcast; downstream forwarders re-sign the envelope (so `Envelope.sender_sig` differs along the path) but the inner `tracker_id` and `tracker_sig` are the issuer's, untouched.

### 5.3 Validator (`shared/federation/validate.go`)

`ValidateRevocation` enforces:
- `tracker_id`, `identity_id` exactly 32 bytes; non-zero.
- `tracker_sig` exactly 64 bytes.
- `revoked_at > 0`.
- `reason` in valid enum range (`UNSPECIFIED < reason ≤ EXPIRED`).

`ValidateEnvelope`'s ceiling lifts to `KIND_REVOCATION`.

### 5.4 Canonical signing (`signing_revocation.go`)

```go
// CanonicalRevocationPreSig zeroes tracker_sig and returns
// signing.DeterministicMarshal of the result. Both issuer and verifier
// reconstruct identically.
func CanonicalRevocationPreSig(m *Revocation) ([]byte, error)
```

## 6. `PeerRevocationArchive` interface

```go
// PeerRevocationArchive is the slice of *storage.Store federation needs
// for revocation gossip. Production binds to *ledger/storage.Store; tests
// pass a fake.
type PeerRevocationArchive interface {
    PutPeerRevocation(ctx context.Context, r storage.PeerRevocation) error
    GetPeerRevocation(ctx context.Context, trackerID, identityID []byte) (storage.PeerRevocation, bool, error)
}
```

The federation `Deps` gains:

```go
type Deps struct {
    // existing
    RevocationArchive PeerRevocationArchive  // nil disables revocation gossip
}
```

If `dep.RevocationArchive == nil`, the coordinator is a no-op: `OnFreeze` does nothing and inbound `KIND_REVOCATION` is rejected with a metric. Slice-0 deployments without the archive continue to work (root-attestation only, no revocations).

## 7. Storage table

`tracker/internal/ledger/storage/schema_v1.sql` additive:

```sql
CREATE TABLE IF NOT EXISTS peer_revocations (
    tracker_id  BLOB NOT NULL,         -- issuer
    identity_id BLOB NOT NULL,         -- revoked identity
    reason      INTEGER NOT NULL,      -- RevocationReason enum value
    revoked_at  INTEGER NOT NULL,      -- unix seconds (issuer's clock)
    tracker_sig BLOB NOT NULL,         -- 64 bytes
    received_at INTEGER NOT NULL,      -- unix seconds (local clock)
    PRIMARY KEY (tracker_id, identity_id)
);

CREATE INDEX IF NOT EXISTS idx_peer_revocations_identity
    ON peer_revocations (identity_id);
```

Per the ledger spec rule "Migrations are append-only: a v2 schema lands as a separate set of statements, never as ALTER on existing v1 tables", this is additive — new tables/indexes only, no existing-table modification.

The composite primary key `(tracker_id, identity_id)` makes idempotency a one-statement insert: `INSERT OR IGNORE`. A retry receives the same revocation and finds the row already present; no double-emit at the federation layer because dedupe catches the message_id first.

`storage.PeerRevocation` mirrors the proto fields:

```go
type PeerRevocation struct {
    TrackerID   []byte // 32
    IdentityID  []byte // 32
    Reason      uint32
    RevokedAt   uint64
    TrackerSig  []byte // 64
    ReceivedAt  uint64
}
```

`Store.PutPeerRevocation` runs the `INSERT OR IGNORE`, returning nil even on duplicate (idempotent). `Store.GetPeerRevocation` reads by `(tracker_id, identity_id)`. `Store.ListRevocationsForIdentity` is deferred — registry/admission integration is the consumer and lives in a follow-up.

## 8. Outbound flow

```
reputation.evaluator.transitionToFrozen(id, reason, now)
  → reputation.Subsystem.dep.FreezeListener.OnFreeze(id, "freeze_repeat", now)
                                                          │
                                                          ▼
federation.Federation.OnFreeze (RevocationCoordinator):
  1. require dep.RevocationArchive != nil. Else metric + no-op.
  2. translate reason string → RevocationReason enum (well-known
     mappings: "freeze_repeat" → REVOCATION_REASON_ABUSE; "operator"
     → REVOCATION_REASON_MANUAL; default → ABUSE for any reputation-
     emitted reason). Mapping table lives in revocation.go and is
     trivially extensible.
  3. build Revocation proto with tracker_id = MyTrackerID, identity_id,
     reason, revoked_at = now.
  4. canonical = CanonicalRevocationPreSig(rev). rev.tracker_sig =
     Ed25519.Sign(MyPriv, canonical).
  5. payload = proto.Marshal(rev).
  6. dedupe.Mark(sha256(payload)).
  7. forward(KIND_REVOCATION, payload). The slice-0 forward closure
     dedupe-marks and broadcasts to all active peers.
  8. Persist locally via PutPeerRevocation so a later restart-and-
     receive of the same revocation from a peer is a no-op.
  9. metric: revocations_emitted_total.
```

## 9. Inbound flow

```
recv KIND_REVOCATION from peer:
  ─── envelope-sig verified by dispatcher; dedupe.Seen-checked
  │
  ▼
RevocationCoordinator.OnIncoming(ctx, env, fromPeer):
  1. parse Revocation from env.Payload.
  2. ValidateRevocation(rev). On fail: drop + metric.
  3. canonical = CanonicalRevocationPreSig(rev).
  4. issuerPub, ok := PeerPubKey(rev.tracker_id). If !ok, drop + metric
     (unknown issuer; v1 trusts only operator-allowlisted peers, same
     as slice 0).
  5. ed25519.Verify(issuerPub, canonical, rev.tracker_sig). On fail:
     drop + metric.
  6. PutPeerRevocation(ctx, storage.PeerRevocation{...}).
     INSERT OR IGNORE; nil error.
  7. forward(KIND_REVOCATION, env.Payload) — the slice-0 forward
     closure dedupes + broadcasts to peers other than fromPeer. (The
     dedupe.Seen short-circuit at the dispatcher's recv path catches
     the round-trip echo.)
  8. metric: revocations_received_total{outcome="archived"}.
```

## 10. Failure handling

| Failure | Behavior |
|---|---|
| `dep.RevocationArchive == nil` deployment | OnFreeze no-ops with metric; inbound KIND_REVOCATION rejected with metric. |
| Issuer pubkey unknown (`PeerPubKey` returns `!ok`) | Drop frame, `invalid_frames{reason="revocation_unknown_issuer"}`. |
| Bad sig on inner `Revocation` | Drop, `invalid_frames{reason="revocation_sig"}`. |
| Bad shape on `Revocation` | Drop, `invalid_frames{reason="revocation_shape"}`. |
| Storage `PutPeerRevocation` returns non-nil | Log + metric. Do NOT forward (we cannot vouch for durability). |
| Reputation listener call panics inside federation | Recover at the outermost `OnFreeze` boundary, log + metric. Reputation evaluator stays alive. |
| Self-revocation received (issuer == MyTrackerID via a peer echo) | Persist + forward as normal (the dedupe core has already caught the round-trip echo at the dispatcher level; this branch just hardens against weird routing). |

## 11. Configuration

No new config fields in `FederationConfig`. The dedupe TTL, gossip rate caps, and send queue depths from slice 0 cover revocations identically to root-attestation.

## 12. Metrics

New Prometheus counters under `tokenbay_federation_`:

- `revocations_emitted_total` — local freezes emitted as `REVOCATION`.
- `revocations_received_total{outcome=archived|sig|shape|unknown_issuer|disabled|storage_err}`.
- `revocations_archive_size` — gauge, optional (deferred unless ops asks).

Existing `invalid_frames_total{reason}` gains `revocation_shape`, `revocation_sig`, `revocation_unknown_issuer`.

## 13. Testing

TDD throughout, one conventional commit per red-green cycle.

1. **Wire format.** `ValidateRevocation` happy path + bad-shape table; `CanonicalRevocationPreSig` deterministic + zeros tracker_sig + tamper-detect.
2. **Unit `RevocationCoordinator.OnFreeze`.** Stub `forward` captures payload; a freeze for a known identity emits a well-formed `Revocation` payload, signed by `MyPriv`, with the right reason mapping, and persists locally.
3. **Unit `RevocationCoordinator.OnIncoming`.** Stub archive captures the persisted row; happy path verifies issuer sig, archives, calls forward (excluding sender).
4. **Bad-issuer-sig drop.** Tampered `tracker_sig` is dropped; archive empty; metric incremented.
5. **Unknown issuer drop.** `PeerPubKey` returns `!ok`; dropped.
6. **Storage layer.** `Put / Get` happy path. Duplicate `Put` is a no-op (`INSERT OR IGNORE`). `Get` for missing returns `ok=false`.
7. **Reputation→federation hook.** Inject a fake `FreezeListener`; trigger an evaluator transition; assert the listener saw the right `(id, reason, time)`.
8. **Two-tracker integration (in-process transport).** Tracker A's reputation freezes identity X; B archives the revocation within 100ms.
9. **Three-tracker forwarding.** Linear A↔B↔C. A emits; assert C archives. Re-emission from C back through B is dedupe'd.
10. **Race-cleanliness.** All tests pass `-race`.

## 14. Open questions

- **Operator-override of advisory revocations** (umbrella spec §6.1). Slice 2 honors all by default. The override knob — e.g. `tracker/internal/admin POST /peer-revocations/<id>/override` — is the operator-admin slice's job.
- **Cryptographic verification against a trusted-issuer directory.** v1 trusts the operator-managed allowlist's pubkey for the issuer. A wider directory would let federation accept revocations from non-allowlisted regional trackers (umbrella spec §7 "bootstrap-after-first-contact" peers). Out of scope.
- **Revocation expiry / un-revoke.** Spec doesn't define one. v1 archive grows monotonically; pruning is operator-driven. A formal un-revoke message (`KIND_UNREVOCATION`?) is out of scope and intentionally not designed for here.
- **Bidirectional listener registration race.** If the reputation evaluator emits a freeze before federation calls `reputation.RegisterFreezeListener`, the freeze is lost from federation's perspective. Mitigation: `cmd/token-bay-tracker/run_cmd.go` orders federation construction before reputation; reputation reads `dep.FreezeListener` at every emit, never caches it.

## 15. Acceptance criteria

- Two `Federation` instances peered on the in-process transport. Tracker A's reputation freezes identity X. B's `peer_revocations` table contains the row within 100ms.
- A three-tracker line A↔B↔C: A emits, C archives, dedupe catches the echo.
- Bad-issuer-sig and unknown-issuer paths drop the message with the right metric labels.
- A `dep.RevocationArchive == nil` deployment of slice 0 is unaffected: existing `ROOT_ATTESTATION` tests still pass, federation `OnFreeze` is a no-op, inbound `KIND_REVOCATION` is rejected with the same.
- `make check` (test + lint) green at repo root, `tracker/`, and `shared/`.
- Federation package continues to pass `go test -race ./...`.

## 16. Subsystem implementation index

- **`tracker/internal/federation` (revocation)** — plan: `docs/superpowers/plans/2026-05-10-federation-revocation-gossip.md` (to be written via the writing-plans skill).

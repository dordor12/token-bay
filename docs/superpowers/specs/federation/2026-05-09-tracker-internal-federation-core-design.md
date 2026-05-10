# Tracker `internal/federation` — Core + Root-Attestation Subsystem Design

| Field | Value |
|---|---|
| Parent | [Federation Protocol](2026-04-22-federation-design.md) |
| Sibling | [Regional Tracker](../tracker/2026-04-22-tracker-design.md) §3, §11 |
| Status | Design draft |
| Date | 2026-05-09 |
| Scope | First vertical slice of `tracker/internal/federation`. Lands the peer registry, a transport-pluggable peer connection model with an in-process default, the minimal HELLO/PEER_AUTH/PEERING_ACCEPT handshake, gossip core (dedupe + forward), and the `ROOT_ATTESTATION` + `EQUIVOCATION_EVIDENCE` message flow end-to-end against the existing ledger storage. Cross-region transfer, revocation gossip, peer exchange, and a real QUIC peer transport are explicitly out of scope and tracked as follow-up subsystems. |

## 1. Purpose

Make a tracker a participant in the federation gossip graph at the level required to publish and verify ledger Merkle roots and to detect equivocation. Provide the core machinery — transport abstraction, peer state machine, message envelope, signature verification, and dedupe-and-forward gossip — that all later federation slices build on.

In one sentence: after this slice, two trackers can peer in-process, exchange `ROOT_ATTESTATION`s, archive each other's roots, and collectively detect a third tracker that publishes two different roots for the same hour.

## 2. Non-goals

- Cross-region credit transfer (`TRANSFER_PROOF_REQUEST`/`TRANSFER_PROOF`/`TRANSFER_APPLIED`).
- Identity revocation gossip (`REVOCATION`).
- Peer exchange and bootstrap (`PEER_EXCHANGE`).
- Health scoring (peer ranking, latency-aware routing).
- Real QUIC/TLS peer transport — replaced by a pluggable `Transport` interface plus an in-process implementation. Real network transport ships in a follow-up subsystem.
- Computing or signing the local Merkle root. Federation pulls a finished `(root, tracker_sig)` for the hour via the `RootSource` interface; the ledger (or a future ledger orchestrator) is responsible for producing it.
- Operator admin-API integration. Federation exposes the necessary read/depeer methods on its public type; routing them through `admin-api` is a small follow-up that does not block this slice.

## 3. Position in the system

Federation is one of the tracker's internal modules listed in the tracker spec §3. It depends on:

- `tracker/internal/ledger/storage` — for `PutPeerRoot` (`ErrPeerRootConflict`) and `GetPeerRoot`.
- `tracker/internal/ledger` (orchestrator) — exposed indirectly through a `RootSource` interface that returns the signed Merkle root for a closed hour. Tests use a stub `RootSource`.
- `shared/federation/` (new package, see §6) — wire format protobufs and shape validators.
- `shared/signing` — Ed25519 signing/verification helpers.
- `shared/ids` — `IdentityID` / `TrackerID` type.
- `tracker/internal/config` — `FederationConfig` is already defined; this slice extends it minimally (see §10).

It is consumed by:

- `cmd/token-bay-tracker/run_cmd.go` — wires the federation subsystem in, like `broker.Open` and `admission.Open`.
- (Later) `tracker/internal/api/transfer_request.go` — the `federationStartTransfer` interface stub already references the federation subsystem; that wiring lands with the cross-region transfer slice, not this one.

## 4. Architecture

### 4.1 Module layout

```
shared/federation/
  federation.proto                ← message protos (see §6)
  federation.pb.go                ← generated
  validate.go / validate_test.go  ← shape validators (called before any state mutation)

tracker/internal/federation/
  doc.go
  errors.go / errors_test.go
  config.go                       ← FederationDeps + Config plumbing (defaults from internal/config)
  transport.go                    ← Transport / PeerConn interfaces
  transport_inproc.go             ← in-process Transport (paired channels)
  transport_inproc_test.go
  peer.go / peer_test.go          ← Peer state machine + per-peer goroutines
  handshake.go / handshake_test.go← HELLO + PEER_AUTH framing & verification
  registry.go / registry_test.go  ← active peers, lookup by tracker_id, depeer
  envelope.go / envelope_test.go  ← outer Envelope + message_id derivation
  dedupe.go / dedupe_test.go      ← TTL'd LRU keyed by message_id
  gossip.go / gossip_test.go      ← apply-locally + forward-to-others
  rootattest.go / rootattest_test.go
  equivocation.go / equivocation_test.go
  publisher.go / publisher_test.go← clock-driven outbound ROOT_ATTESTATION emit
  subsystem.go / subsystem_test.go← Open(cfg, deps) → *Federation; Start/Close
  metrics.go / metrics_test.go
  integration_test.go             ← two-tracker scenarios over in-process transport
```

One file per area of responsibility, mirroring `internal/broker` and `internal/session`.

### 4.2 Public type

```go
// Federation is the per-tracker federation subsystem. Open returns a
// running instance bound to the given Transport, peer config, RootSource,
// and ledger storage. Lifecycle: Open → Close.
type Federation struct { /* … */ }

func Open(cfg Config, deps Deps) (*Federation, error)
func (f *Federation) Close() error

// Operator-facing reads.
func (f *Federation) Peers() []PeerInfo
func (f *Federation) Depeer(trackerID ids.TrackerID, reason DepeerReason) error

// Driver entry point for the publisher.
func (f *Federation) PublishHour(ctx context.Context, hour uint64) error
```

`Config` carries the dedupe TTL, gossip-rate caps, hour cadence, peer list, and the local tracker identity. `Deps` carries `Transport`, `RootSource`, `PeerRootArchive`, `Logger`, `Metrics`, `Now func() time.Time`. The split mirrors `broker.Open` — Config is YAML-derivable, Deps is wired in code.

### 4.3 Concurrency model

- One goroutine per peer connection (`recvLoop`).
- One outbound goroutine per peer connection drains an unbuffered `Send` channel; the bounded send queue is the back-pressure point.
- `dedupe` is a single TTL LRU behind a `sync.Mutex` (low write rate; cheap).
- `registry` is sharded by `hash(tracker_id) % shards` with `sync.RWMutex` per shard, mirroring `internal/registry`.
- `publisher` is a single goroutine driven by an injected ticker.
- All packages MUST pass `go test -race`. Federation is on the always-`-race` list per the umbrella tracker spec §6 / admission-design §11.3.

## 5. Interfaces

### 5.1 `Transport` and `PeerConn`

```go
// Transport is the connection plane between trackers. The slice ships an
// in-process implementation; a real QUIC/TLS transport implements the
// same interface in a follow-up subsystem and drops in by composition.
type Transport interface {
    Dial(ctx context.Context, addr string, expectedPeer ed25519.PublicKey) (PeerConn, error)
    Listen(ctx context.Context, accept func(PeerConn)) error
    Close() error
}

type PeerConn interface {
    Send(ctx context.Context, frame []byte) error  // length-delimited frame, 1 MiB cap
    Recv(ctx context.Context) ([]byte, error)
    RemoteAddr() string
    RemotePub() ed25519.PublicKey
    Close() error
}
```

Frames are length-prefixed (4-byte big-endian length, then payload, hard cap 1 MiB). The transport does NOT speak protobuf — that is the `envelope` layer's job.

### 5.2 `RootSource`

```go
// RootSource yields the local tracker's signed Merkle root for the
// closed hour. The returned (root, sig) bytes are exactly what
// storage.PutMerkleRoot persisted; federation does not re-sign.
//
// ok=false means the root is not yet ready (e.g. the orchestrator has
// not produced it). The publisher logs and retries on the next tick.
type RootSource interface {
    ReadyRoot(ctx context.Context, hour uint64) (root, sig []byte, ok bool, err error)
}
```

For tests, a stub `RootSource` returns deterministic bytes.

### 5.3 `PeerRootArchive`

```go
// PeerRootArchive is the slice of *storage.Store federation needs.
type PeerRootArchive interface {
    PutPeerRoot(ctx context.Context, p storage.PeerRoot) error
    GetPeerRoot(ctx context.Context, trackerID []byte, hour uint64) (storage.PeerRoot, bool, error)
}
```

Backed in production by `*ledger/storage.Store`. The interface keeps test wiring lean and makes the dependency explicit.

## 6. Wire format — `shared/federation/`

A new package mirroring `shared/admission/`. Lives in `shared/` because peers exchange these messages across the network, per CLAUDE.md repo rule §3 and the tracker CLAUDE.md note about wire-format types.

`federation.proto` (proto3, package `tokenbay.federation.v1`):

```proto
// Outer envelope. Every steady-state message rides inside one.
message Envelope {
  bytes  sender_id   = 1;  // 32 bytes — sender's tracker_id
  Kind   kind        = 2;
  bytes  payload     = 3;  // serialized inner message
  bytes  sender_sig  = 4;  // 64 bytes — Ed25519(payload) by sender
}

enum Kind {
  KIND_UNSPECIFIED         = 0;
  KIND_HELLO               = 1;
  KIND_PEER_AUTH           = 2;
  KIND_PEERING_ACCEPT      = 3;
  KIND_PEERING_REJECT      = 4;
  KIND_ROOT_ATTESTATION    = 5;
  KIND_EQUIVOCATION_EVIDENCE = 6;
  KIND_PING                = 7;
  KIND_PONG                = 8;
}

message Hello {
  bytes    tracker_id        = 1;  // 32 bytes
  uint32   protocol_version  = 2;  // start at 1
  bytes    nonce             = 3;  // 32 random bytes; counterparty signs in PeerAuth
  repeated string features   = 4;  // forward-compat feature flags; ignore unknown
}

message PeerAuth {
  bytes nonce_sig = 1;  // 64 bytes — Ed25519(peer's Hello.nonce) with our tracker key
}

message PeeringAccept {
  uint32 dedupe_ttl_s   = 1;
  uint32 gossip_rate_qps = 2;  // sender's offered receive rate cap
}

message PeeringReject {
  string reason = 1;  // "version", "blocklisted", "ratelimit", …
}

message RootAttestation {
  bytes  tracker_id   = 1;  // 32 bytes — must match Envelope.sender_id
  uint64 hour         = 2;  // unix-epoch hour (== timestamp_seconds / 3600)
  bytes  merkle_root  = 3;  // exactly the bytes stored in merkle_roots.root
  bytes  tracker_sig  = 4;  // exactly merkle_roots.tracker_sig (Ed25519)
}

message EquivocationEvidence {
  bytes  tracker_id = 1;  // offender
  uint64 hour       = 2;
  bytes  root_a     = 3;
  bytes  sig_a      = 4;
  bytes  root_b     = 5;
  bytes  sig_b      = 6;
}

message Ping { uint64 nonce = 1; }
message Pong { uint64 nonce = 1; }
```

`shared/federation/validate.go` enforces (callable from any tracker):

- Field lengths (`tracker_id`/`merkle_root`/`sig` exactly 32/32/64 bytes; `nonce` 32 bytes).
- `kind` in valid enum range.
- `payload` non-empty for kinds that require it.
- For `RootAttestation`: `tracker_id` non-zero, `hour` non-zero.
- For `EquivocationEvidence`: `root_a != root_b`, both sigs 64 bytes.

Bad shapes are rejected with typed errors before any state mutation.

## 7. Peer state machine

States: `DIALED → HELLO_SENT → AUTH_SENT → STEADY → CLOSED`.
Symmetric on the listener side: `ACCEPTED → HELLO_RECVD → AUTH_VERIFIED → STEADY → CLOSED`.

```
DIALED ──Send Hello──▶ HELLO_SENT ──Recv Hello──▶ AUTH_SENT ──Recv PeerAuth (verify)──▶ STEADY
                                                                                          │
                                                                              CLOSED ◀──disconnect/depeer
```

Handshake details:

1. Both sides send `Hello{tracker_id, protocol_version=1, nonce, features}` immediately on connect.
2. Each side verifies the counterparty's `tracker_id` against the peer config (operator-managed allowlist for v1; mismatched id → reject and close).
3. Each side sends `PeerAuth{nonce_sig = Ed25519(counterparty.nonce)}`. Recipient verifies against the counterparty's known pubkey (which is the same as `tracker_id`).
4. If both sides accept, send `PeeringAccept` and transition to `STEADY`. Otherwise `PeeringReject` and close.

Handshake timeout: 5 s (configurable). Failure logs + metric, no retry inside this slice — the operator's peer list dictates dial retry policy at the subsystem boundary.

## 8. Gossip core

### 8.1 Envelope and `message_id`

`message_id = sha256(payload_bytes)`. The `Envelope` header itself is not part of `message_id` — this guarantees that the same inner message gossiped via two paths dedupes correctly even though `sender_sig` differs.

Outbound:

1. Caller hands `gossip.Forward(payload_bytes, kind, exclude PeerID)` an already-serialized inner message and the kind.
2. Gossip computes `message_id`, signs `payload_bytes` with the local tracker key to produce `sender_sig`, then ships the envelope to every active peer except `exclude`.
3. `dedupe.Mark(message_id)` so that if a peer echoes the same message back, we drop it on receipt.

Inbound:

1. `peer.recvLoop` decodes the envelope, validates shape (`shared/federation/validate.go`), verifies `sender_sig` against the peer's Ed25519 pubkey.
2. `dedupe.Seen(message_id)` short-circuits replays.
3. Dispatch by `kind`:
   - `KIND_ROOT_ATTESTATION` → `rootattest.Apply` (§9.2).
   - `KIND_EQUIVOCATION_EVIDENCE` → `equivocation.Receive` (§9.4).
   - `KIND_PING` → reply with `Pong{nonce}`. `KIND_PONG` → record peer liveness.
   - Handshake kinds (`KIND_HELLO`/`PEER_AUTH`/`PEERING_*`) post-`STEADY` → drop with a metric (handshake completed; receiving these again is anomalous).
   - Future kinds added to the enum that this binary does not yet recognize → drop with a metric (forward-compat: parse the envelope, skip the body).

### 8.2 Dedupe TTL

Default 1 h (`FederationConfig.GossipDedupeTTLS`, already in `internal/config`). Implemented as a map keyed by `message_id` plus a min-heap of `(expires_at, message_id)` pulled lazily on `Seen`/`Mark`. Bounded size cap (default 64 K entries) evicts oldest on overflow.

### 8.3 Rate limits

Per-peer token-bucket on outbound `Send` (default 100 frames/s). On overflow, drop frame + metric. This is a defense-in-depth measure; the structural rate cap is the bounded send queue (default 256). Open question §10.5 of the umbrella spec — values are tunable and subject to revision.

## 9. ROOT_ATTESTATION flow

### 9.1 Outbound (publisher)

```
ticker (hourly, clock-injected)
  │
  ▼
publisher.Tick(now):
  1. hour := uint64(now.Unix() / 3600) - 1   # the just-closed hour
  2. (root, sig, ok, err) := RootSource.ReadyRoot(ctx, hour)
     - err          → log + metric, no broadcast.
     - !ok          → log at debug + metric, retry next tick.
  3. payload := MarshalProto(RootAttestation{tracker_id, hour, root, sig})
  4. dedupe.Mark(sha256(payload))
  5. gossip.Forward(payload, KIND_ROOT_ATTESTATION, exclude=nil)
```

The publisher never re-publishes a previously-published hour from federation's side — the dedupe Mark step handles round-trip echoes from peers.

### 9.2 Inbound

```
rootattest.Apply(env, payload):
  msg := UnmarshalProto[RootAttestation](payload)
  1. validate(msg)                                  # field lengths, non-zero
  2. require msg.tracker_id == env.sender_id        # an attestation about you came from you
  3. err := archive.PutPeerRoot(ctx, PeerRoot{
                tracker_id: msg.tracker_id,
                hour:       msg.hour,
                root:       msg.merkle_root,
                sig:        msg.tracker_sig,
                received_at: now(),
            })
  4. switch err:
       nil                    → gossip.Forward(payload, KIND_ROOT_ATTESTATION, exclude=peer)
       ErrPeerRootConflict    → equivocation.Handle(msg, peer)   # see §9.3
       other                  → log + metric, drop.
```

Verification of the tracker's own signature on `tracker_sig` over `(merkle_root, hour)` is the ledger orchestrator's responsibility once it lands; for this slice, federation accepts the bytes as opaque and lets storage's idempotency rules be the source of truth. (Adding a Verify hook is a one-line change once the canonical signing helper exists in `shared/signing`.)

### 9.3 Equivocation

```
equivocation.Handle(incoming, fromPeer):
  1. (existing, ok, err) := archive.GetPeerRoot(ctx, incoming.tracker_id, incoming.hour)
     - !ok or err → log + metric (we just got ErrPeerRootConflict, but the row vanished — race; ignore).
  2. evidence := EquivocationEvidence{
         tracker_id: incoming.tracker_id,
         hour:       incoming.hour,
         root_a:     existing.root, sig_a: existing.sig,
         root_b:     incoming.merkle_root, sig_b: incoming.tracker_sig,
     }
  3. payload := MarshalProto(evidence)
  4. dedupe.Mark(sha256(payload))
  5. gossip.Forward(payload, KIND_EQUIVOCATION_EVIDENCE, exclude=nil)
  6. registry.Depeer(incoming.tracker_id, ReasonEquivocation)
```

### 9.4 Receiving `EquivocationEvidence` from a peer

Dispatched from §8.1 on `KIND_EQUIVOCATION_EVIDENCE`:

```
equivocation.Receive(env, payload):
  evi := UnmarshalProto[EquivocationEvidence](payload)
  validate(evi)                                    # root_a != root_b, sigs 64B
  if evi.tracker_id == localTrackerID:
      metric.equivocations_about_self_total++
      log.Critical("equivocation about us; investigate keys/clock")
      # no auto-action — operator decides
  else:
      gossip.Forward(payload, KIND_EQUIVOCATION_EVIDENCE, exclude=fromPeer)
      if registry.IsActive(evi.tracker_id):
          registry.Depeer(evi.tracker_id, ReasonEquivocation)
```

We do not re-verify the embedded sigs against an authoritative pubkey in this slice — accepting evidence on its face is sufficient to depeer; doing the cryptographic verification before honoring evidence is a hardening item flagged for the reputation slice.

## 10. Configuration

`tracker/internal/config.FederationConfig` already declares peer counts, dedupe TTL, and transfer-retry window. This slice extends it minimally:

```go
type FederationConfig struct {
    PeerCountMin         int    // existing
    PeerCountMax         int    // existing
    GossipDedupeTTLS     int    // existing — used as dedupe TTL
    TransferRetryWindowH int    // existing — unused in this slice

    // New in this slice:
    HandshakeTimeoutS    int      // default 5
    GossipRateQPS        int      // default 100
    SendQueueDepth       int      // default 256
    PublishCadenceS      int      // default 3600 (one hour); test override expected
    Peers                []Peer   // operator-managed allowlist
}

type Peer struct {
    TrackerID  string  // hex-encoded 32 bytes
    Addr       string  // transport-specific (e.g. "inproc://eu-1")
    PubKey     string  // hex-encoded Ed25519
    Region     string  // human-friendly hint; advisory
}
```

`apply_defaults.go` and `validate.go` get the corresponding entries; `validate.go` enforces non-empty hex, well-formed lengths, and non-overlapping `TrackerID`s in the peer list.

## 11. Failure handling

| Failure | Behavior |
|---|---|
| Handshake timeout / version mismatch | Send `PeeringReject{reason}`, close, log, metric. No retry inside this slice. |
| Bad envelope shape | Drop frame, increment `federation_invalid_frames_total{reason}`. |
| Bad signature on envelope | Drop frame, increment metric, do NOT close — could be a transient corruption. Repeated bad sigs from one peer (>10 in 60 s) triggers depeer. |
| `archive.PutPeerRoot` returns a non-conflict error | Log + metric. Do NOT forward (we cannot vouch for storage durability). |
| `RootSource.ReadyRoot` returns `!ok` | Log at debug, retry next tick. |
| Peer disconnect | `peer.recvLoop` exits; `registry.Remove(trackerID)`; outbound queue drained and closed. Operator's peer list will redial on the next reconciliation cycle (deferred to the real transport slice). |
| Self-equivocation alert | Critical-severity metric + log; no automatic action. |

## 12. Metrics

Prometheus, prefix `tokenbay_federation_`:

- `peers{state}` — gauge of peers by state (`steady`, `dialing`, `closed`).
- `frames_in_total{kind}` / `frames_out_total{kind}`.
- `invalid_frames_total{reason}` — bad shape, bad sig, dedupe replay (separately useful).
- `dedupe_size` — gauge.
- `root_attestations_published_total`.
- `root_attestations_received_total{outcome}` — `archived`, `replay`, `conflict`, `error`.
- `equivocations_detected_total`.
- `equivocations_about_self_total` — alert SLO.

## 13. Testing strategy

TDD throughout, one conventional commit per red-green cycle. Per-file unit tests plus a focused `integration_test.go`:

1. **Two-tracker happy path.** Two `Federation` instances on the in-process transport. Tracker A's publisher emits a `RootAttestation` for hour H. Assert tracker B has the row in its `PeerRootArchive`.
2. **Forwarding.** Three trackers in a line A↔B↔C. A publishes; assert C archives; assert dedupe drops a re-broadcast.
3. **Equivocation.** B has previously archived A's root for hour H. A third peer (controlled stub) feeds B a different root + sig for the same `(A, H)`. Assert B emits `EquivocationEvidence` to all peers except the source, depeers the source, and logs.
4. **Bad signature.** Stub peer sends an envelope with a tampered `sender_sig`; assert the frame is dropped and metric incremented.
5. **Handshake.** Version mismatch yields `PeeringReject` and clean close. Wrong nonce signature yields the same.
6. **Race-cleanliness.** Run all tests under `-race`. The package is on the always-`-race` list.

The in-process transport is exposed in a `transport_inproc.go` that can also be reused by other tracker integration tests.

## 14. Open questions

- **Self-emission protection.** If a publisher misfires twice for the same hour, dedupe avoids redundant gossip but the local Merkle write is in storage's hands (`PutMerkleRoot` is non-idempotent today). Out of scope here; flagged for the ledger orchestrator subsystem.
- **Real transport handoff.** The shape of `Transport.Listen(accept func(PeerConn))` is influenced by what the QUIC slice will need (mTLS-derived `RemotePub`, `Listen` blocking vs. spawning). The signature in §5.1 is sufficient for the in-process transport; we will revisit when the QUIC slice starts.
- **Operator admin endpoints.** `Federation.Peers()` and `Depeer` are public on the type; routing through `admin-api` as `GET /peers`, `POST /peers/remove` lands in a tiny follow-up. Not blocking.
- **Reputation impact of equivocation.** Spec-level "reputation hit" is acknowledged via metric + log here; the actual reputation-write path lands when the reputation subsystem does.

## 15. Acceptance criteria

- Two `Federation` instances peer over the in-process transport, exchange `ROOT_ATTESTATION`s, and persist each other's roots via `storage.PutPeerRoot`.
- An equivocating third tracker is detected and depeered within one gossip round, and `EquivocationEvidence` reaches all other peers.
- All tests pass `go test -race ./...` for `tracker/internal/federation` and `shared/federation`.
- `make check` (test + lint) is green.
- The federation subsystem is wired into `cmd/token-bay-tracker/run_cmd.go` with the in-process transport defaulting to disabled (operator opts in via config peer list); a non-empty peer list activates it.
- The existing `api/transfer_request.go` stub (which references `federationStartTransfer`) compiles unchanged — this slice does not touch the cross-region transfer interface.

## 16. Subsystem implementation index

- **`tracker/internal/federation`** — plan `docs/superpowers/plans/2026-05-09-tracker-internal-federation-core.md` (to be written via the writing-plans skill).

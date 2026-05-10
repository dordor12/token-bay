# Federation — Cross-Region Credit Transfer Subsystem Design

| Field | Value |
|---|---|
| Parent | [Federation Protocol](2026-04-22-federation-design.md), [Federation Core + Root-Attestation](2026-05-09-tracker-internal-federation-core-design.md) |
| Status | Design draft |
| Date | 2026-05-10 |
| Scope | Implements §4 of the umbrella federation spec: the `TRANSFER_PROOF_REQUEST` / `TRANSFER_PROOF` / `TRANSFER_APPLIED` message flow that lets a consumer routed to destination tracker Y spend credits whose home is source tracker X. Adds the federation-level wire format, per-peer unicast send primitive, source- and destination-side handlers, the `LedgerHooks` dependency, and a `Federation.StartTransfer` entry point that satisfies the existing `federationStartTransfer` interface in `tracker/internal/api`. |

## 1. Purpose

Make `tracker/internal/federation` the protocol carrier for cross-region credit transfer. After this slice:

- A consumer's plugin can ask a non-home tracker Y to broker. Y's federation calls X's federation. X debits the consumer locally (`transfer_out`), signs a `TransferProof`, and returns it. Y appends a matching `transfer_in` and confirms.
- The umbrella spec §10 acceptance criterion *"a cross-region credit transfer works end-to-end"* becomes satisfiable end-to-end at the federation layer. Plugin-side fallback orchestration (the broker-result decision to reach for federation transfer) and the API handler implementation are sibling slices outside this scope but pluggable into the new `Federation.StartTransfer` entry point.

## 2. Non-goals

- Implementing the full `tracker/internal/api/transfer_request.go` HTTP/RPC handler. Federation exposes `StartTransfer(ctx, in) → (out, err)`; api wires it. The api-side validation (consumer-sig, balance pre-check, RPC framing) stays in `internal/api` as a follow-up.
- Plugin-side broker-fallback decision (when to reach for cross-region transfer vs. accept `OutcomeNoCapacity`). That belongs in the plugin's broker-orchestration slice.
- Adding `consumer_sig` and `consumer_pub` to the existing RPC `tbproto.TransferRequest`. Internal mapping happens in api; federation accepts the consumer-sig + consumer-pub as explicit input on `StartTransfer`. The proto change ships with the api slice.
- Persistent restart-survival of the source-side nonce-replay cache (idempotent retry across tracker restarts). v1 keeps the cache in memory; restart drops it. Documented as a §11 caveat and §14 open question; persistence ships with a follow-up that adds an indexed `transfer_out` lookup by `TransferRef` to ledger storage.
- Operator-facing admin endpoints for transfer state (in-flight, completed). Metrics + logs only.
- Negotiating multi-source transfers (gathering credit from > 1 home tracker for a single request). Single-source-per-request only.
- Reversal flow (the §4.3 "24h escalation back to X to reverse"). Out of scope; flagged in §14.

## 3. Position in the system

This subsystem extends `tracker/internal/federation`. It depends on:

- `shared/federation/` — extends the proto with three new `Kind` values and three new messages (§5).
- `tracker/internal/federation` — existing `Transport`, `PeerConn`, `Registry`, `Gossip`, `Dedupe`, `Envelope`, dispatcher (`Federation.makeDispatcher`).
- `tracker/internal/ledger` — extended with a new `Ledger.AppendTransferIn` method (§9). The existing `AppendTransferOut` is reused as-is.
- `shared/signing` — `DeterministicMarshal` + `SignEntry` / `VerifyEntry` for the consumer-sig path; new canonical-bytes helpers for `TransferProofRequest`, `TransferProof`, `TransferApplied` per the shared CLAUDE.md rule §6.
- `shared/ids` — `TrackerID`, `IdentityID`.

It is consumed by:

- `tracker/internal/api/transfer_request.go` — already declares the `federationStartTransfer` interface stub. After this slice, the api handler can call `federation.StartTransfer` directly. (The api handler's full implementation is a sibling follow-up.)

## 4. Architecture

### 4.1 Module layout (additions only)

```
shared/federation/
  federation.proto                    ← MODIFY: +3 Kind values, +3 messages
  federation.pb.go                    ← regenerated
  validate.go / validate_test.go      ← MODIFY: validators for the 3 new messages
  signing_transfer.go                 ← NEW: canonical-bytes helpers for the 3 messages
  signing_transfer_test.go

tracker/internal/federation/
  transfer.go / transfer_test.go      ← NEW: TransferCoordinator (StartTransfer + handlers)
  ledger_hooks.go                     ← NEW: LedgerHooks interface + small adapter type
  unicast.go / unicast_test.go        ← NEW: per-peer Send-by-trackerID primitive
  subsystem.go                        ← MODIFY: wire TransferCoordinator + LedgerHooks dep
  config.go                           ← MODIFY: TransferTimeoutS + new defaults
  metrics.go / metrics_test.go        ← MODIFY: transfer_* counters
  integration_test.go                 ← MODIFY: add 2-tracker source/dest scenarios

tracker/internal/ledger/
  transfer.go                         ← MODIFY: add AppendTransferIn
  transfer_test.go                    ← MODIFY: TestAppendTransferIn happy path + dup
```

Mirrors the one-file-per-area-of-responsibility pattern of slice 0 (`rootattest.go`, `equivocation.go`, …).

### 4.2 New public types

```go
// StartTransferInput is what the api handler hands to federation. The
// handler has already parsed the RPC request and verified the consumer
// signature against the canonical-bytes derived from the same fields;
// federation re-checks the sig over the wire-format canonical bytes
// before forwarding (defense in depth, and the source tracker re-checks
// when the request lands).
type StartTransferInput struct {
    SourceTrackerID  ids.TrackerID
    IdentityID       ids.IdentityID
    Amount           uint64
    Nonce            [32]byte           // also used as TransferRef in the ledger
    ConsumerSig      []byte             // 64 bytes
    ConsumerPub      ed25519.PublicKey  // 32 bytes
    Timestamp        uint64             // unix seconds, picked by the api handler
}

type StartTransferOutput struct {
    SourceChainTipHash [32]byte
    SourceSeq          uint64
    SourceTrackerSig   []byte // 64
}

func (f *Federation) StartTransfer(ctx context.Context, in StartTransferInput) (StartTransferOutput, error)
```

`StartTransfer` is called at the *destination* tracker Y. The first time a particular `(SourceTrackerID, Nonce)` pair is asked for, federation:

1. Resolves the active peer for `SourceTrackerID` (`registry.IsActive`); if not steady, returns `ErrPeerNotConnected`.
2. Builds + canonicalizes a `TransferProofRequest`, signs the envelope, sends via the unicast primitive (§7).
3. Awaits the `TransferProof` response on a per-nonce delivery channel (timeout = `cfg.TransferTimeout`, default 30 s).
4. Verifies `source_tracker_sig` against the source peer's known pubkey from the registry.
5. Calls `LedgerHooks.AppendTransferIn(ctx, …)`. If this returns `ledger.ErrTransferRefExists`, treats as success (idempotent retry): the credit was already booked; pull the prior chain seq for completeness.
6. Sends `TransferApplied` (signed by Y's tracker key) back to source via unicast (best-effort; source's `transfer_out` is already final by §4.3 invariant).
7. Returns `StartTransferOutput` to the caller.

A retry of `StartTransfer` with the same `(SourceTrackerID, Nonce)` while the first call is still in-flight rides the same delivery channel.

### 4.3 Concurrency model

- A single `TransferCoordinator` per `Federation` holds two maps:
  - `pending map[Nonce]chan *fed.TransferProof` — in-flight requests issued at this tracker (destination role).
  - `issued map[Nonce]issuedProof` — proofs already minted at this tracker (source role), for replay-on-retry. LRU bound `cfg.IssuedProofCap = 4096`.
- Both maps live behind a single `sync.Mutex`. Lookup volume is low (one map op per transfer message); contention is not a concern.
- The pending map is keyed by `Nonce` alone. Two different sources colliding on the same nonce is astronomically unlikely with 32-byte nonces; if it happens (test vectors, replay), the first delivery wins and the second hits the timeout — a logged anomaly, not a correctness break.
- Background timeout enforcement is per-call: each waiter selects on `ctx.Done()` and a `time.After(cfg.TransferTimeout)`.
- All paths must pass `go test -race` (federation is on the always-`-race` list).

## 5. Wire format additions — `shared/federation/`

### 5.1 New `Kind` enum values

```proto
enum Kind {
  // existing values 0..8 unchanged
  KIND_TRANSFER_PROOF_REQUEST = 9;
  KIND_TRANSFER_PROOF         = 10;
  KIND_TRANSFER_APPLIED       = 11;
}
```

### 5.2 New messages

```proto
message TransferProofRequest {
  bytes  source_tracker_id = 1;  // 32 bytes
  bytes  dest_tracker_id   = 2;  // 32 bytes (the destination — sender of this request)
  bytes  identity_id       = 3;  // 32 bytes
  uint64 amount            = 4;
  bytes  nonce             = 5;  // 32 bytes — also serves as ledger TransferRef
  bytes  consumer_sig      = 6;  // 64 bytes — Ed25519(canonical) by consumer
  bytes  consumer_pub      = 7;  // 32 bytes — consumer's pubkey
  uint64 timestamp         = 8;  // unix seconds; replay window enforced by issued-cache TTL
}

message TransferProof {
  bytes  source_tracker_id     = 1;  // 32 bytes
  bytes  dest_tracker_id       = 2;  // 32 bytes
  bytes  identity_id           = 3;  // 32 bytes
  uint64 amount                = 4;
  bytes  nonce                 = 5;  // 32 bytes — same as request
  bytes  source_chain_tip_hash = 6;  // 32 bytes — X's ledger tip after AppendTransferOut
  uint64 source_seq            = 7;  // X's seq for the new transfer_out entry
  uint64 timestamp             = 8;  // X's commit timestamp, unix seconds
  bytes  source_tracker_sig    = 9;  // 64 bytes — Ed25519(canonical) by X's tracker
}

message TransferApplied {
  bytes  source_tracker_id = 1;  // 32 bytes
  bytes  dest_tracker_id   = 2;  // 32 bytes
  bytes  nonce             = 3;  // 32 bytes
  uint64 timestamp         = 4;  // unix seconds, dest commit
  bytes  dest_tracker_sig  = 5;  // 64 bytes — Ed25519(canonical) by Y's tracker
}
```

### 5.3 Validators (`validate.go`)

`ValidateTransferProofRequest`, `ValidateTransferProof`, `ValidateTransferApplied` enforce:
- All 32-byte fields exactly 32 bytes; all 64-byte sigs exactly 64 bytes.
- `amount > 0`.
- `source_tracker_id != dest_tracker_id`.
- `source_tracker_id`, `dest_tracker_id`, `identity_id`, `nonce` non-zero.
- `timestamp > 0`.

`ValidateEnvelope` is extended to accept the new `Kind` ceiling (`Kind <= KIND_TRANSFER_APPLIED`).

### 5.4 Canonical signing (`signing_transfer.go`)

Per shared CLAUDE.md §6, every signed proto goes through `shared/signing.DeterministicMarshal`. Three helpers:

```go
func CanonicalTransferProofRequestPreSig(m *TransferProofRequest) ([]byte, error) // zeroes consumer_sig
func CanonicalTransferProofPreSig(m *TransferProof) ([]byte, error)              // zeroes source_tracker_sig
func CanonicalTransferAppliedPreSig(m *TransferApplied) ([]byte, error)          // zeroes dest_tracker_sig
```

Each clears the relevant signature field, calls `signing.DeterministicMarshal`, and returns the bytes the signer signs over. The verifier reconstructs identically.

## 6. `LedgerHooks` interface

```go
// LedgerHooks is the federation-side view of the ledger orchestrator. It
// is the single dependency that lets federation commit cross-region
// transfers without dragging in the full *ledger.Ledger surface.
type LedgerHooks interface {
    // AppendTransferOut debits Amount from IdentityID's balance and
    // records a transfer_out entry with TransferRef = Nonce. Returns
    // the chain-tip hash, the new entry's seq, and the timestamp at
    // which the entry was committed (== r.Timestamp on success).
    //
    // Returns ErrTransferRefExists if a prior transfer_out with the
    // same TransferRef is already on the chain. v1 federation drops
    // the duplicate request and emits a metric; v2 (post-§14 follow-up)
    // walks the ledger to reconstruct the prior proof and replay it.
    AppendTransferOut(ctx context.Context, in TransferOutHookIn) (TransferOutHookOut, error)

    // AppendTransferIn credits Amount to IdentityID's balance and
    // records a transfer_in entry with TransferRef = Nonce. Idempotent:
    // returns nil for first-apply AND for already-applied (we cannot
    // double-credit). Returns ledger.ErrTransferRefExists when the row
    // is already there; the caller (StartTransfer) treats this as
    // success and proceeds with TRANSFER_APPLIED.
    AppendTransferIn(ctx context.Context, in TransferInHookIn) error
}

type TransferOutHookIn struct {
    IdentityID  [32]byte
    Amount      uint64
    Timestamp   uint64
    TransferRef [32]byte           // = Nonce
    ConsumerSig []byte             // 64
    ConsumerPub ed25519.PublicKey  // 32
}

type TransferOutHookOut struct {
    ChainTipHash [32]byte
    Seq          uint64
}

type TransferInHookIn struct {
    IdentityID  [32]byte
    Amount      uint64
    Timestamp   uint64
    TransferRef [32]byte
}
```

Production binding: a thin adapter wraps `*ledger.Ledger`. The adapter is constructed in `cmd/token-bay-tracker/run_cmd.go` at the same site that wires the existing `RootSrc` dependency.

`Deps` gains:

```go
type Deps struct {
    // existing
    Ledger LedgerHooks  // NEW — required for transfer flow; nil disables StartTransfer
}
```

If `dep.Ledger == nil`, `StartTransfer` returns `ErrTransferDisabled`; inbound `KIND_TRANSFER_PROOF_REQUEST` frames are rejected with the same error and a metric. Slice-0 deployments without the ledger hook continue to work for root-attestation only.

## 7. Per-peer unicast (`unicast.go`)

Slice 0's `Gossip.Forward` broadcasts to all active peers; cross-region transfer is point-to-point. `unicast.go` adds:

```go
// SendToPeer signs an envelope and writes it to the named peer's send
// queue. Returns ErrPeerNotConnected if the peer is not in steady state
// or is not in the active map. Does not gossip-forward; the caller is
// responsible for any subsequent broadcast.
func (f *Federation) SendToPeer(ctx context.Context, peerID ids.TrackerID, kind fed.Kind, payload []byte) error
```

Implementation: lookup `f.peers[peerID]` under `f.mu`, then call `Peer.Send(ctx, frame)`. Same `MaxFrameBytes = 1 MiB` cap as broadcast. Re-uses `SignEnvelope` from the gossip layer.

The dedupe Mark is **not** done for unicast (these messages are not gossiped onward). The destination-side dispatcher still calls `dedupe.Seen` on receive — a malicious peer that re-broadcasts a unicast frame is dropped at the recipient.

## 8. Source-side flow (X)

```
recv KIND_TRANSFER_PROOF_REQUEST from Y
  ─── envelope-sig verified, dedupe.Seen-checked, payload validated
  │
  ▼
TransferCoordinator.OnRequest(ctx, env, msg):
  1. fast-path replay: issued[msg.nonce] hit → SendToPeer(Y, TransferProof, cached); return.
  2. require msg.dest_tracker_id == env.sender_id (the requester == us-as-peer).
  3. require msg.source_tracker_id == cfg.MyTrackerID (the request was for us).
  4. verify consumer_sig: canonical = CanonicalTransferProofRequestPreSig(msg);
     ed25519.Verify(consumer_pub, canonical, consumer_sig) → false → drop + metric.
  5. call dep.Ledger.AppendTransferOut(ctx, TransferOutHookIn{...}).
     err == ErrTransferRefExists → walk-and-replay (§14 deferred); for v1, drop + metric.
     other err → drop + metric.
  6. build TransferProof:
        source_tracker_id     = MyTrackerID
        dest_tracker_id       = msg.dest_tracker_id
        identity_id           = msg.identity_id
        amount                = msg.amount
        nonce                 = msg.nonce
        source_chain_tip_hash = out.ChainTipHash
        source_seq            = out.Seq
        timestamp             = msg.timestamp
     canonical = CanonicalTransferProofPreSig(proof);
     proof.source_tracker_sig = ed25519.Sign(myTrackerPriv, canonical).
  7. issued[msg.nonce] = {payloadBytes, expiresAt: now + cfg.TransferTimeout * 2}.
  8. SendToPeer(Y, KIND_TRANSFER_PROOF, marshal(proof)).
  9. metric: transfers_minted_total.
```

## 9. Destination-side flow (Y)

```
StartTransfer(ctx, in):
  1. require dep.Ledger != nil.
  2. require registry has SourceTrackerID active.
  3. ch := pending.register(in.Nonce); defer pending.deregister(in.Nonce).
  4. build TransferProofRequest{... in.* ...}, marshal canonical, embed consumer_sig.
  5. SendToPeer(SourceTrackerID, KIND_TRANSFER_PROOF_REQUEST, payload).
  6. select:
       <-ctx.Done()                  → return ErrCanceled.
       <-time.After(cfg.TransferTimeout) → return ErrTransferTimeout.
       proof := <-ch                 → continue.
  7. verify proof.source_tracker_sig (peer pubkey from registry).
  8. err := dep.Ledger.AppendTransferIn(ctx, TransferInHookIn{
            Amount, Timestamp, TransferRef: nonce}).
       err in {nil, ErrTransferRefExists} → treat as success.
       other err → return wrapped err.
  9. build + sign TransferApplied, SendToPeer(SourceTrackerID, KIND_TRANSFER_APPLIED, ...).
       error here is logged + metric only (the credit is already booked).
 10. return StartTransferOutput{proof.source_chain_tip_hash, proof.source_seq, proof.source_tracker_sig}.
```

```
recv KIND_TRANSFER_PROOF from X (dispatcher → TransferCoordinator.OnProof):
  1. envelope sig already verified.
  2. parse + validate proof.
  3. require msg.dest_tracker_id == cfg.MyTrackerID.
  4. require msg.source_tracker_id == env.sender_id.
  5. ch, ok := pending[msg.nonce]; !ok → drop + metric (orphan or late).
  6. ch <- msg (non-blocking; buffer 1).
```

```
recv KIND_TRANSFER_APPLIED from Y (dispatcher → TransferCoordinator.OnApplied):
  1. envelope sig already verified.
  2. parse + validate applied.
  3. require msg.source_tracker_id == cfg.MyTrackerID.
  4. require dest_tracker_sig verifies against env.sender_id's known pubkey.
  5. metric: transfers_confirmed_total. log debug.
```

## 10. Ledger addition: `AppendTransferIn`

Mirrors `AppendTransferOut` minus the consumer-sig requirement (this entry is tracker-signed only):

```go
type TransferInRecord struct {
    PrevHash    []byte // 32
    Seq         uint64
    Amount      uint64
    Timestamp   uint64
    TransferRef []byte // 32
}

func (l *Ledger) AppendTransferIn(ctx context.Context, r TransferInRecord) (*tbproto.Entry, error)
```

Internally:
1. `entry.BuildTransferInEntry(...)` to produce the body.
2. `l.appendEntry(ctx, appendInput{body: body, deltas: []balanceDelta{{identityID: ?, delta: +amount}}})`.
   - The `identityID` for the credit is the *consumer's* identity. But `BuildTransferInEntry` zero-fills `consumer_id` per the entry spec (transfer_in is bookkeeping, not consumer-attributed). The balance projection therefore needs the identity carried alongside the body; we pass `IdentityID` as an extra field on `TransferInRecord` and route the delta directly. Concretely: extend `TransferInRecord` with `IdentityID []byte` (32B) used only for the balance delta, not the body — the on-chain entry remains zero-filled.

Idempotency: if a `transfer_in` entry with the same `TransferRef` is already present, `AppendTransferIn` returns `ErrTransferRefExists`. v1 implements this with a small chain-tip-walking check inside `transfer.go`; an indexed lookup is the storage-layer follow-up flagged in §14.

## 11. Failure handling

| Failure | Behavior |
|---|---|
| Source peer not connected at `StartTransfer` | Return `ErrPeerNotConnected`; api handler maps to `503` or equivalent. |
| Source-side `AppendTransferOut` returns `ErrInsufficientCredits` | Source drops the request + metric (`transfer_request_rejected_total{reason="insufficient"}`) — no `KIND_TRANSFER_PROOF` is sent; destination times out. Future: a `KIND_TRANSFER_REJECT` message (deferred §14). |
| Source-side `AppendTransferOut` returns `ErrTransferRefExists` | v1: drop + metric. v2 (post-§14): walk ledger to reconstruct proof and replay. |
| Destination receives unsolicited `KIND_TRANSFER_PROOF` | Drop + metric (`transfer_proof_orphan_total`). |
| Destination `AppendTransferIn` returns non-`ErrTransferRefExists` error | `StartTransfer` returns wrapped error; consumer's plugin retries (idempotent at the next `StartTransfer`). |
| `KIND_TRANSFER_APPLIED` lost (network drop) | Source-side keeps the issued entry as-is; missing applied is a logged metric only. The applied is purely a confirmation for source-side observability. |
| Bad proto shape on any of the three | Drop + `invalid_frames{reason=…}` metric. |
| Bad signature (envelope or inner) | Drop + `invalid_frames{reason="sig"}`. |
| Slice deployed with `dep.Ledger == nil` | `StartTransfer` returns `ErrTransferDisabled`; inbound `KIND_TRANSFER_PROOF_REQUEST` is rejected the same way (with metric). |
| Tracker restart with in-flight transfers | Pending channels are dropped at shutdown; in-progress callers see `ErrCanceled` from their ctx. Restart drops the issued-cache; a retry of the same nonce against the fresh process double-debits at the source until the persistent index lands (§14). |

## 12. Configuration

`tracker/internal/config.FederationConfig` extensions (matching the existing pattern in `apply_defaults.go` and `validate.go`):

```go
TransferTimeoutS  int  // default 30,  range [1, 600]
IssuedProofCap    int  // default 4096, range [128, 1<<20]
```

Applied in `federation.Config.withDefaults`.

## 13. Metrics

New Prometheus counters under `tokenbay_federation_`:

- `transfer_request_sent_total` — destination-side `StartTransfer` invocations.
- `transfer_request_received_total{outcome=ok|insufficient|sig|stale}` — source-side `OnRequest` outcomes.
- `transfer_proof_received_total{outcome=delivered|orphan|sig}` — destination-side `OnProof`.
- `transfer_applied_received_total{outcome=ok|sig|orphan}` — source-side `OnApplied`.
- `transfers_minted_total` — successful proofs the tracker has issued (source role).
- `transfer_completed_total` — successful `StartTransfer` returns (destination role).
- `transfer_failed_total{reason=peer_unreachable|timeout|sig|ledger|disabled}`.

Existing `invalid_frames_total{reason}` gains the labels `transfer_request_shape`, `transfer_proof_shape`, `transfer_applied_shape`.

## 14. Open questions

- **Persistent issued-proof cache.** Today the source-side `issued` map is in-memory; restart loses replay. The fix is an indexed `transfer_out` lookup by `TransferRef` in `tracker/internal/ledger/storage`. Out of scope here.
- **`KIND_TRANSFER_REJECT`.** A negative response (`insufficient_credits`, `revoked_identity`, etc.) avoids the destination waiting for the full timeout. Useful but not required for the §10 happy path; ships when the broker-fallback flow lands and we can measure the latency cost.
- **Replay across distinct (source, dest) pairs.** A consumer-signed request is bound by `consumer_sig`'s coverage of `(source, dest, identity, amount, nonce, timestamp)`; sending the same payload to a different `dest_tracker_id` would fail at the source's `dest_tracker_id == env.sender_id` check. Documented; covered by tests.
- **Reversal flow** (umbrella spec §4.3, 24h escalation). Out of scope.
- **Nonce collisions across simultaneous sources.** With 32-byte nonces the probability is negligible. If observed, switch the pending map to `(source, nonce)` — costs ~16 bytes per entry. Defer until measured.

## 15. Acceptance criteria

- Two `Federation` instances peered on the in-process transport. Y calls `Federation.StartTransfer` for an identity whose home is X. The flow completes end-to-end:
  - X has a new `transfer_out` ledger entry debiting the consumer.
  - Y has a new `transfer_in` ledger entry crediting the same identity.
  - Y's `StartTransfer` returns the source proof.
  - X has logged `KIND_TRANSFER_APPLIED` received and incremented the metric.
- A retried `StartTransfer` with the same `(SourceTrackerID, Nonce)` returns the cached proof (without a second debit) on the source.
- A bad consumer-sig request is dropped at the source with the right metric label; the destination times out cleanly.
- A `dep.Ledger == nil` deployment of slice 0 is unaffected: existing `ROOT_ATTESTATION` tests still pass, `StartTransfer` returns `ErrTransferDisabled`, inbound transfer kinds are rejected with the same.
- `make check` (test + lint) green from `tracker/`, `shared/`, repo root.
- Federation package continues to pass `go test -race ./...`.

## 16. Subsystem implementation index

- **`tracker/internal/federation` (transfer)** — plan: `docs/superpowers/plans/2026-05-10-federation-cross-region-transfer.md` (to be written via the writing-plans skill).

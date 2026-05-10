# Federation — Plugin Bootstrap Signing Subsystem Design

| Field | Value |
|---|---|
| Parent | [Federation Protocol](2026-04-22-federation-design.md), [Peer Exchange](2026-05-10-federation-peer-exchange-design.md) |
| Status | Design draft |
| Date | 2026-05-10 |
| Scope | Implements §7.2 of the umbrella federation spec: a plugin-facing signed snapshot of the tracker's top-N known-peers (by `health_score`). Adds an RPC method (`RPC_METHOD_BOOTSTRAP_PEERS`), a wire envelope (`BootstrapPeerList`), canonical-bytes + sign/verify helpers in `shared/proto/`, an `api` handler reading `tracker/internal/ledger/storage.ListKnownPeers(ctx, limit, byHealthDesc=true)` from slice 3, and a `plugin/internal/trackerclient` method that verifies the signature and returns the parsed list. §7.3 (real `health_score` computation) and downstream plugin reconnection/rerouting using the verified list are sibling slices and out of scope. |

## 1. Purpose

Make every regional tracker capable of issuing a **signed bootstrap snapshot** to plugins on demand, so plugins can grow their known-peers table beyond the 3–5 hardcoded entries in their config (umbrella spec §7.2). After this slice:

- A plugin connected to any one tracker can call `RPC_METHOD_BOOTSTRAP_PEERS` and receive a `BootstrapPeerList { issuer_id, signed_at, expires_at, peers[], sig }` whose `sig` is an Ed25519 signature by the issuing tracker's identity key over `DeterministicMarshal` of the list with `sig=nil`.
- The plugin verifies the signature against the same Ed25519 pubkey it already trusted to dial that tracker, checks `expires_at > now()` with bounded clock-skew tolerance, and returns a `[]BootstrapPeer` to the caller.
- The umbrella spec §10 acceptance criterion *"Peer-exchange populates the plugin's known-peers table to ≥ 50 trackers within 1 hour of bootstrap in a network of that size"* becomes end-to-end satisfiable: slice 3 fills the tracker-side `known_peers` table; slice 4 lets a plugin pull that table verifiably.

## 2. Non-goals

- **Plugin-side persistence/use of the verified list.** Slice 4 stops at "verify and return". The actual rerouting logic (rank by latency, retry against the new pool, persist between sidecar restarts) is a future plugin-side feature. Slice 4 only guarantees the data flows and is verifiable.
- **Real `health_score` computation** (umbrella spec §7.3). Slice 4 reads whatever `health_score` slice 3 wrote into `known_peers` (placeholder values for v1: `1.0` connected-allowlist, `0.5` disconnected-allowlist, gossip rows verbatim). Slice 5 replaces the placeholder.
- **Cross-signed bootstrap chain.** Umbrella spec §3.2 mentions a "bootstrap release key" for the cross-signed handshake-CA chain — that is for *inter-tracker* TLS chain validation, not per-tracker plugin-bootstrap signing. v1 uses each tracker's own identity Ed25519 key; no parent CA is involved.
- **On-disk snapshot caching.** Tracker computes the snapshot on demand each call (cheap — bounded SQL query + ~50 entries × Ed25519 sign). Plugin caller decides whether to cache the result; the trackerclient method does not.
- **Periodic / push refresh.** Plugin asks when it wants; tracker does not push and does not schedule a periodic emit. (Compare to slice 3, which has a periodic `PublishPeerExchange` on the federation channel — that is tracker-to-tracker, not tracker-to-plugin.)
- **Per-entry signing.** Only the outer envelope is signed. Individual `BootstrapPeer` entries inside the list are advisory data, identical to how slice 3's `KnownPeer` entries are advisory.
- **Issuer self-row.** The tracker does NOT include itself in the returned `peers[]`. The plugin already knows the issuing tracker (it's the one it asked). Including it would be redundant and creates an unnecessary self-attestation surface.

## 3. Position in the system

This subsystem extends the **plugin-tracker RPC** layer (`shared/proto/`, `tracker/internal/api`, `plugin/internal/trackerclient`). It is not a federation protocol message — it crosses the plugin↔tracker boundary, not the tracker↔tracker boundary.

It depends on:

- `shared/proto/rpc.proto` — extended with `RPC_METHOD_BOOTSTRAP_PEERS`, `BootstrapPeersRequest`, `BootstrapPeer`, `BootstrapPeerList`.
- `shared/proto/validate.go` — extended with `ValidateBootstrapPeerList`, `MaxBootstrapPeers`, etc.
- `shared/proto/canonical.go` (new) — `CanonicalBootstrapPeerListPreSig`, `SignBootstrapPeerList`, `VerifyBootstrapPeerListSig`.
- `tracker/internal/ledger/storage.ListKnownPeers(ctx, limit, byHealthDesc)` — slice 3's existing method (signature: `ListKnownPeers(ctx context.Context, limit int, byHealthDesc bool) ([]KnownPeer, error)`); slice 4 calls it with `byHealthDesc=true`.
- `tracker/internal/api.Router` — installs the new handler if `Deps.BootstrapPeers != nil`.
- `plugin/internal/trackerclient` — adds `Client.FetchBootstrapPeers` invoking the RPC.
- `shared/signing` — already provides `DeterministicMarshal` (used inside the canonical helper).

It is consumed by:

- (Future) plugin-side rerouting logic that ranks peers by latency and uses them as fallback dial targets when the bootstrap entries are unreachable.

## 4. Architecture

### 4.1 Module layout

```
shared/proto/
  rpc.proto                    ← MODIFY: +1 RpcMethod, +3 messages
  rpc.pb.go                    ← regenerated
  validate.go / validate_test.go
                               ← MODIFY: ValidateBootstrapPeerList + caps
  canonical.go / canonical_test.go
                               ← NEW: CanonicalBootstrapPeerListPreSig + Sign/Verify

tracker/internal/api/
  bootstrap_peers.go           ← NEW: handler + BootstrapPeersService iface
  bootstrap_peers_test.go      ← NEW
  router.go                    ← MODIFY: install BOOTSTRAP_PEERS handler
  router_test.go               ← MODIFY: add OK + nil-deps cases
  errors.go                    ← MODIFY (if needed): no new error codes expected

cmd/token-bay-tracker/
  run_cmd.go                   ← MODIFY: build BootstrapPeersService adapter,
                                          pass into api.Deps

plugin/internal/trackerclient/
  bootstrap_peers.go           ← NEW: Client.FetchBootstrapPeers
  bootstrap_peers_test.go      ← NEW
  types.go                     ← MODIFY: BootstrapPeer struct (plugin-side)
```

One file per area of responsibility, mirroring slice 3's pattern.

### 4.2 New public types

```go
// shared/proto (proto-generated):
type BootstrapPeer struct {
    TrackerId   []byte  // 32
    Addr        string  // "host:port" UDP for QUIC
    RegionHint  string
    HealthScore float64
    LastSeen    uint64  // unix seconds
}

type BootstrapPeerList struct {
    IssuerId   []byte           // 32 — issuing tracker's IdentityID
    SignedAt   uint64           // unix seconds
    ExpiresAt  uint64           // unix seconds
    Peers      []*BootstrapPeer
    Sig        []byte           // 64 — Ed25519 over Canonical(...PreSig)
}

type BootstrapPeersRequest struct{} // empty for v1
```

```go
// tracker/internal/api/bootstrap_peers.go (new file):
type BootstrapPeersService interface {
    ListKnownPeers(ctx context.Context, opts storage.ListKnownPeersOpts) ([]storage.KnownPeer, error)
    IssuerID() ids.IdentityID                      // tracker's identity
    Sign(canonical []byte) ([]byte, error)         // wraps the identity Ed25519 key
}

// plugin/internal/trackerclient/types.go (extended):
type BootstrapPeer struct {
    TrackerID   ids.IdentityID
    Addr        string
    RegionHint  string
    HealthScore float64
    LastSeen    time.Time
}
```

### 4.3 Where signing happens

The tracker's identity Ed25519 keypair already lives in the federation `Config.MyPriv` / `Config.MyPub`, used by `transfer.go`, `revocation.go`, `envelope.go`, and the slice-3 peer-exchange envelope sender. The api-level `BootstrapPeersService` adapter (built in `cmd/token-bay-tracker/run_cmd.go`) closes over the same `crypto/ed25519.PrivateKey` value. No new key material is introduced.

Plugin-side: `trackerclient.Client` already has the connected tracker's `TrackerEndpoint.IdentityHash` (SHA-256 of SPKI) from its bootstrap config. The QUIC handshake additionally exposes the raw pubkey on connection (the tracker presents its identity cert; the client validates it matches `IdentityHash`). The bootstrap-peers verifier uses that already-validated pubkey directly — no extra key lookup, no extra trust step.

## 5. Wire format additions

### 5.1 `shared/proto/rpc.proto`

```proto
enum RpcMethod {
  // ... existing ...
  RPC_METHOD_BOOTSTRAP_PEERS = 10;
}

message BootstrapPeersRequest {}

message BootstrapPeer {
  bytes  tracker_id   = 1;  // 32
  string addr         = 2;
  string region_hint  = 3;
  double health_score = 4;
  uint64 last_seen    = 5;
}

message BootstrapPeerList {
  bytes  issuer_id  = 1;  // 32
  uint64 signed_at  = 2;  // unix seconds
  uint64 expires_at = 3;  // unix seconds
  repeated BootstrapPeer peers = 4;
  bytes  sig        = 5;  // 64 — Ed25519 over Canonical(list)
}
```

The `RpcResponse.Payload` carries `proto.Marshal(BootstrapPeerList)`. Status is `RPC_STATUS_OK` on success, `RPC_STATUS_INTERNAL` on storage error, `RPC_STATUS_INVALID` only if a future request shape adds parameters that can be invalid (none in v1).

### 5.2 Validator caps

```go
// shared/proto/validate.go
const (
    MaxBootstrapPeers              = 256
    MaxBootstrapPeerAddrLen        = 256
    MaxBootstrapPeerRegionHintLen  = 64
    MaxBootstrapPeerListLifetimeS  = 60 * 60         // 1h sanity ceiling
    BootstrapPeerListSkewToleranceS = 60             // plugin-side
)

func ValidateBootstrapPeerList(l *tbproto.BootstrapPeerList) error {
    // - l != nil
    // - len(l.IssuerId) == 32
    // - len(l.Sig) == ed25519.SignatureSize (64)
    // - l.SignedAt > 0
    // - l.ExpiresAt > l.SignedAt
    // - l.ExpiresAt - l.SignedAt <= MaxBootstrapPeerListLifetimeS
    // - len(l.Peers) <= MaxBootstrapPeers
    // - for each peer: tracker_id 32 bytes, addr utf-8 and len ≤ cap,
    //   region_hint utf-8 and len ≤ cap, health_score in [0,1] (NaN rejected)
}
```

Plugin-side staleness check (in `trackerclient.FetchBootstrapPeers`, not in the validator):

```go
if uint64(now.Unix()) > list.ExpiresAt + BootstrapPeerListSkewToleranceS {
    return nil, ErrBootstrapPeerListExpired
}
```

### 5.3 Canonical helper

`shared/proto/canonical.go` (new file — a lightweight peer to slice-2's `shared/federation/canonical.go`, but for plugin-tracker signed messages):

```go
// CanonicalBootstrapPeerListPreSig returns the bytes signed by the
// issuer. It is DeterministicMarshal of the list with Sig cleared.
func CanonicalBootstrapPeerListPreSig(list *tbproto.BootstrapPeerList) ([]byte, error) {
    if list == nil { return nil, errors.New("proto: nil BootstrapPeerList") }
    clone := proto.Clone(list).(*tbproto.BootstrapPeerList)
    clone.Sig = nil
    return signing.DeterministicMarshal(clone)
}

func SignBootstrapPeerList(priv ed25519.PrivateKey, list *tbproto.BootstrapPeerList) error {
    cb, err := CanonicalBootstrapPeerListPreSig(list)
    if err != nil { return err }
    list.Sig = ed25519.Sign(priv, cb)
    return nil
}

func VerifyBootstrapPeerListSig(pub ed25519.PublicKey, list *tbproto.BootstrapPeerList) error {
    if len(pub) != ed25519.PublicKeySize {
        return errors.New("proto: bad pubkey")
    }
    sig := list.Sig
    cb, err := CanonicalBootstrapPeerListPreSig(list)
    if err != nil { return err }
    if !ed25519.Verify(pub, cb, sig) {
        return ErrBootstrapPeerListBadSig
    }
    return nil
}
```

`ErrBootstrapPeerListBadSig` is a sentinel exported from `shared/proto`. Naming follows the `MaxRPCPayloadSize` precedent (`shared/CLAUDE.md` notes hand-written wrappers use uppercase initialism `RPC` while protoc-generated identifiers retain `Rpc`).

## 6. `BootstrapPeersService` interface

```go
// tracker/internal/api/bootstrap_peers.go
type BootstrapPeersService interface {
    ListKnownPeers(ctx context.Context, limit int, byHealthDesc bool) ([]storage.KnownPeer, error)
    IssuerID() ids.IdentityID
    Sign(canonical []byte) ([]byte, error)
}
```

Why an interface, not `*storage.Store` directly: the handler also needs `IssuerID()` and `Sign()`, which live on the federation/identity layer, not on storage. The composition root in `cmd/token-bay-tracker/run_cmd.go` builds a small adapter struct that delegates `ListKnownPeers` to the SQLite store and `Sign`/`IssuerID` to closures over the existing identity material.

## 7. Storage queries

No new table — slice 4 reads the slice 3 `known_peers` table.

The handler calls:

```go
// Over-fetch by 1 so removing the issuer's self-row (if present) still
// leaves cfg.MaxPeers entries.
peers, err := svc.ListKnownPeers(ctx, cfg.MaxPeers+1, true /* byHealthDesc */)
```

The handler then **filters out the issuer's own row** (in case slice 3's allowlist seed wrote one) and **truncates** the remaining list to `cfg.MaxPeers`. Self-row filtering is in the api layer because it is a plugin-facing concern, not a storage invariant; slice 5 may have reasons to keep the row inside `known_peers` for health bookkeeping.

## 8. Tracker-side flow

```
plugin RPC      api.Router.Dispatch(BOOTSTRAP_PEERS)
                ↓
                bootstrapPeersHandler
                ↓
                svc.ListKnownPeers(ctx, N+1, byHealthDesc=true)
                ↓
                drop self-row, truncate to N
                ↓
                build BootstrapPeerList{ issuer_id=svc.IssuerID(), signed_at, expires_at, peers }
                ↓
                cb := CanonicalBootstrapPeerListPreSig(list)
                list.Sig = svc.Sign(cb)
                ↓
                payload, _ := proto.Marshal(list)
                return RpcResponse{ Status=OK, Payload=payload }
```

Empty `known_peers` is a valid result: `peers: []` is OK. The signature still covers `signed_at`, `expires_at`, and the empty list.

## 9. Plugin-side flow

```
caller          Client.FetchBootstrapPeers(ctx)
                ↓
                send RpcRequest{ Method=BOOTSTRAP_PEERS, Payload=marshal(BootstrapPeersRequest{}) }
                ↓
                wait for RpcResponse
                ↓
                if Status != OK: return error
                list := proto.Unmarshal(resp.Payload)
                ValidateBootstrapPeerList(list)
                ↓
                pub := c.connectedTrackerPubkey()  // already validated by QUIC handshake
                expectedHash := sha256(SPKI(pub))
                if expectedHash != c.endpoint.IdentityHash: return ErrIdentityMismatch
                ↓
                VerifyBootstrapPeerListSig(pub, list)
                ↓
                if uint64(now.Unix()) > list.ExpiresAt + skew: ErrBootstrapPeerListExpired
                ↓
                return []BootstrapPeer{ ...convert from proto type... }
```

The `connectedTrackerPubkey()` step is the only piece that touches the existing trackerclient internals — it already exists (the QUIC TLS handshake validates `TrackerEndpoint.IdentityHash` against the cert's SPKI). The bootstrap-peers verifier reuses the same already-validated pubkey, so no new trust path is introduced.

## 10. Failure handling

| Failure | Outcome |
|---|---|
| Storage error (SQL failure) | RPC returns `RPC_STATUS_INTERNAL` with `code=BOOTSTRAP_LIST_STORAGE`. Tracker logs `bootstrap_peers_list_err`. |
| Sign error (signer returns error — should never happen with stdlib Ed25519) | RPC returns `RPC_STATUS_INTERNAL` with `code=BOOTSTRAP_LIST_SIGN`. |
| Validator rejects a malformed list at the plugin (caps exceeded, NaN health, bad sig length) | Plugin returns `ErrBootstrapPeerListInvalid`. Tracker should not produce these — they indicate either tampering or a future-version skew. |
| Bad sig at plugin | Plugin returns `ErrBootstrapPeerListBadSig`, surfaces metric `outcome=sig_invalid`. Caller decides whether to retry against a different bootstrap entry. |
| Expired snapshot | Plugin returns `ErrBootstrapPeerListExpired`, surfaces metric `outcome=expired`. The 60-second skew tolerance means a tracker with a clock 30 s ahead of plugin still produces accepted lists. |
| Empty `peers[]` | Returned to caller as `[]BootstrapPeer{}` with no error. Metric `outcome=ok` (caller decides what to do with an empty list). |
| RPC transport error (stream closed mid-call, etc.) | Plugin returns the underlying transport error unchanged. Not counted toward `bootstrap_peers_fetched_total{outcome=...}`. |

## 11. Configuration

```yaml
# tracker config
federation:
  bootstrap:
    max_peers: 50    # default; range [1, 256]
    ttl: 10m         # default; range [1m, 1h]
```

`max_peers` clamps any request size; v1 has no per-call N parameter, so the cap *is* the request size.

`ttl` is the snapshot lifetime. 10 minutes matches the §6.1 signed-balance-snapshot precedent. Operator may shorten it (small networks where membership changes fast) or lengthen up to 1 hour (must not exceed `MaxBootstrapPeerListLifetimeS` validator cap).

Plugin-side configuration: none needed — `BootstrapPeerListSkewToleranceS = 60s` is a compile-time constant.

## 12. Metrics

| Name | Type | Labels | Where |
|---|---|---|---|
| `tokenbay_tracker_bootstrap_peers_served_total` | Counter | — | tracker handler increments on each successful response |
| `tokenbay_tracker_bootstrap_peers_list_size` | Gauge | — | tracker handler sets to the size of the returned `peers[]` |
| `tokenbay_tracker_bootstrap_peers_errors_total` | Counter | `code` (`storage`, `sign`) | tracker handler error path |
| `tokenbay_plugin_bootstrap_peers_fetched_total` | Counter | `outcome` (`ok`, `sig_invalid`, `expired`, `invalid`, `empty`) | plugin trackerclient |

The plugin metrics use the existing trackerclient metrics handle (no new registry plumbing).

## 13. Testing

### 13.1 Unit (shared/proto)

- `Test_CanonicalBootstrapPeerListPreSig_Stable`: build two lists with identical fields, ensure canonical bytes are byte-identical.
- `Test_CanonicalBootstrapPeerListPreSig_ExcludesSig`: set `Sig` to two different values, ensure canonical bytes match.
- `Test_SignAndVerifyBootstrapPeerList`: round-trip with a fresh keypair.
- `Test_VerifyBootstrapPeerListSig_RejectsTampered`: flip a byte in `Addr`, ensure `ErrBootstrapPeerListBadSig`.
- `Test_VerifyBootstrapPeerListSig_RejectsWrongPubkey`: sign with key A, verify with key B → `ErrBootstrapPeerListBadSig`.
- `Test_ValidateBootstrapPeerList_*`: rejection tests for each cap (entry count, addr len, region_hint len, health_score NaN/out-of-range, lifetime > 1h, sig len).

### 13.2 Unit (tracker api handler)

- `TestBootstrapPeers_OK`: fake `BootstrapPeersService` returns 3 entries, handler signs, response validates against the fake's pubkey, list has 3 entries (none of which is issuer).
- `TestBootstrapPeers_FiltersSelf`: one of the storage rows has `tracker_id == issuer`, handler omits it, returned list is 2 entries.
- `TestBootstrapPeers_TruncatesToMax`: storage returns 100 entries, config N=50, handler returns 50.
- `TestBootstrapPeers_Empty`: storage returns nil, handler returns OK with empty `peers`.
- `TestBootstrapPeers_StorageError`: storage returns error, handler returns `RPC_STATUS_INTERNAL` with `code=BOOTSTRAP_LIST_STORAGE`.
- `TestBootstrapPeers_NilDeps`: router with `Deps.BootstrapPeers == nil` returns `notImpl` for the kind.
- `TestBootstrapPeers_TTLBounds`: handler-built list has `expires_at - signed_at == cfg.TTL`.

### 13.3 Unit (plugin trackerclient)

- `TestFetchBootstrapPeers_OK`: fake transport returns a valid signed list, parser produces the expected `[]BootstrapPeer`.
- `TestFetchBootstrapPeers_BadSig`: tampered payload → `ErrBootstrapPeerListBadSig`.
- `TestFetchBootstrapPeers_Expired`: `expires_at = now - 120s` (past skew tolerance) → `ErrBootstrapPeerListExpired`.
- `TestFetchBootstrapPeers_SkewTolerance`: `expires_at = now - 30s` (within skew) → OK.
- `TestFetchBootstrapPeers_IdentityMismatch`: signed by a key whose SPKI hash doesn't match `endpoint.IdentityHash` → `ErrIdentityMismatch`.
- `TestFetchBootstrapPeers_InvalidPayload`: tracker returns garbled bytes → `ErrBootstrapPeerListInvalid`.
- `TestFetchBootstrapPeers_RpcError`: tracker returns `Status=INTERNAL` → caller-visible error.

### 13.4 Integration

One end-to-end test in `tracker/internal/api/bootstrap_peers_test.go` (or a small e2e spike) wiring:

- A real `*storage.Store` seeded via `UpsertKnownPeer` with 5 allowlist rows.
- A real `*api.Router` with `BootstrapPeersService` adapter.
- A real `crypto/ed25519` keypair driving both the signer and the verifier.

This catches integration bugs at the api/storage boundary that the fakes can't (e.g., `OrderByHealthDesc` constant mismatch with the SQL clause).

The full plugin↔tracker e2e (real QUIC + real trackerclient + real router) is **not** part of this slice — that lives in `plugin/test/e2e` and depends on infrastructure that's already shared across slices. The integration test above is sufficient for slice 4's correctness gate.

## 14. Open questions

- **Per-call N parameter.** `BootstrapPeersRequest{}` is empty in v1; the tracker decides N from config. A future version may allow plugins to request smaller N (e.g., low-resource devices) without a wire break — adding `optional uint32 max_peers = 1` is backward-compatible.
- **Multi-tracker fan-out.** A plugin connected to tracker A might benefit from also asking tracker B for *its* known-peers, then de-duping. v1 expects callers to do this themselves (one fetch per dial); a higher-level "discover network" helper is plugin-side product surface.
- **Snapshot caching at the tracker.** v1 computes on-demand. If load profile shows hot-path concerns, an in-memory cache keyed by `(now / TTL)` with copy-on-build is a non-breaking optimization.
- **Identity-rotation interaction.** If the tracker rotates its identity key (separate spec, `tracker/internal/identity` future work), plugins holding a snapshot signed by the old key remain trusted within the snapshot's TTL. After expiry, the plugin re-fetches and learns the new pubkey on the connection that signed the new snapshot. No additional cross-signing is needed at this layer.
- **DDoS surface.** A plugin that hammers `BOOTSTRAP_PEERS` could waste tracker CPU on Ed25519 signs (~50 µs each). Standard tracker rate-limit (existing `tokenbay_tracker_rate_limited_total` machinery) covers this generically; no slice-4-specific limit needed.

## 15. Acceptance criteria

- A plugin can call `Client.FetchBootstrapPeers(ctx)` against a tracker that has ≥1 known-peer row and receive a parsed `[]BootstrapPeer` with a verified signature.
- An empty `known_peers` table returns OK with an empty list (no error).
- A tampered response (signature flipped, peer added, expiry extended) is rejected by the plugin with the correct sentinel error.
- A snapshot whose `expires_at` is more than 60 s in the past (per plugin clock) is rejected with `ErrBootstrapPeerListExpired`.
- Race-clean under `go test -race ./...` across `shared`, `tracker`, `plugin`. Federation coverage stays ≥ slice-3's 76.1%.
- Lint clean (`golangci-lint run ./...`).
- The handler is wired in `cmd/token-bay-tracker/run_cmd.go` so a real tracker built with `make build` serves the RPC end-to-end.
- Existing slice-3 acceptance criteria still pass (no regressions in `peerexchange_test.go`, no schema-migration breakage in `known_peers_test.go`).

## 16. Subsystem implementation index

- `shared/proto/rpc.proto` — wire format additions (§5.1).
- `shared/proto/validate.go` — `ValidateBootstrapPeerList` + caps (§5.2).
- `shared/proto/canonical.go` (NEW) — `Canonical*PreSig`, `Sign*`, `Verify*Sig` (§5.3).
- `tracker/internal/api/bootstrap_peers.go` (NEW) — handler + `BootstrapPeersService` iface (§6, §8).
- `tracker/internal/api/router.go` — install handler (§4.1).
- `cmd/token-bay-tracker/run_cmd.go` — `BootstrapPeersService` adapter wiring (§4.3).
- `plugin/internal/trackerclient/bootstrap_peers.go` (NEW) — `Client.FetchBootstrapPeers` (§9).
- `plugin/internal/trackerclient/types.go` — plugin-side `BootstrapPeer` struct.

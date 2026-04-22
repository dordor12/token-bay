# Federation Protocol — Subsystem Design Spec

| Field | Value |
|---|---|
| Parent | [Token-Bay Architecture](../2026-04-22-token-bay-architecture-design.md) |
| Status | Design draft |
| Date | 2026-04-22 |
| Scope | The wire protocol and state machine by which regional trackers peer with each other. Carries Merkle-root attestations, cross-region credit transfers, identity-revocation gossip, and peer-exchange for bootstrap. |

## 1. Purpose

Make separately-operated regional trackers interoperable so that:

- A consumer in region X can spend credits earned in region Y.
- Ledger rewrites at any tracker are detectable by peers.
- Identity revocations propagate network-wide.
- New plugins can discover more trackers than those on the bootstrap list.

Federation is explicitly **not** distributed consensus — each tracker remains authoritative over its own ledger. Federation provides cross-checking and value transfer.

## 2. Topology

Pairwise peerings, no central coordinator. Graph structure:

- Each tracker maintains `peer_count` (default 8–16) long-lived connections to other trackers.
- Peering accept/reject is an operator decision; operators can also whitelist.
- Gossip propagates via flood-with-deduplication along the peering graph.
- Expected graph diameter: ≤ 4 hops for a network of ~100 trackers.

## 3. Wire protocol

### 3.1 Transport

Long-lived QUIC (or TLS/TCP) connection, mutually authenticated via Ed25519-derived tracker identity certs signed by a peering handshake.

### 3.2 Peering handshake

1. `HELLO {tracker_id, protocol_version, cert_chain, supported_features}` — both sides.
2. Each side verifies cert chain roots to the bootstrap release key (or a peer-provided cross-signed chain).
3. `PEER_AUTH {nonce_sig}` — sign counterparty's nonce with tracker identity key.
4. `PEERING_ACCEPT {peer_params}` or `PEERING_REJECT {reason}`.

On accept, both sides transition to steady-state gossip mode.

### 3.3 Message types (steady state)

All messages are length-prefixed protobuf (or CBOR); choice deferred.

- `ROOT_ATTESTATION {tracker_id, hour, merkle_root, sig}` — per-tracker hourly.
- `TRANSFER_PROOF_REQUEST {source, identity, amount, dest, nonce, consumer_sig}` — a request from a consumer delegated via their home tracker.
- `TRANSFER_PROOF {source, identity, amount, dest, nonce, source_chain_tip_hash, source_tracker_sig}` — returned by source tracker.
- `TRANSFER_APPLIED {dest, source, nonce, dest_tracker_sig}` — confirmation from destination back to source.
- `REVOCATION {tracker_id, identity_id, reason, revoked_at, tracker_sig}` — gossiped widely.
- `PEER_EXCHANGE {peers_known: [{tracker_id, addr, last_seen}]}` — gossip of discovered peers for bootstrap-after-first-contact.
- `EQUIVOCATION_EVIDENCE {tracker_id, hour, root_a, sig_a, root_b, sig_b}` — emergency broadcast.
- `PING / PONG` — keepalive.

### 3.4 Gossip rules

- Each message carries a `message_id = hash(payload)`; dedupe cache TTL 1 hour.
- On receive: verify signatures, apply to local state if applicable, forward to all other peers except the sender.
- Out-of-order delivery allowed; each message type is idempotent.

## 4. Cross-region credit transfer

### 4.1 Motivation

A consumer's identity is registered in region X (their home tracker). They travel and end up routed to region Y (lower latency). They want to spend credits earned in X through Y.

### 4.2 Flow

1. Consumer's plugin asks Y's tracker: "I want to `broker_request` but my balance is in X." Plugin provides a signed `transfer_request` for the amount needed + nonce.
2. Y's tracker asks X's tracker: `TRANSFER_PROOF_REQUEST{source=X, identity, amount, dest=Y, nonce, consumer_sig}`.
3. X's tracker validates: consumer_sig checks, consumer has `amount` + overhead available, the transfer_request nonce hasn't been used. Appends an **outbound transfer entry** (`kind=transfer_out`) to X's ledger debiting `amount` from the identity. Returns `TRANSFER_PROOF{…, X_tracker_sig}`.
4. Y's tracker receives the proof, verifies the signature, appends a **matching inbound entry** (`kind=transfer_in`, `ref = nonce`) to Y's ledger crediting `amount`.
5. Y's tracker sends `TRANSFER_APPLIED` back to X.
6. Consumer's plugin can now broker_request against Y with the updated balance snapshot from Y.

### 4.3 Ordering invariant

The outbound entry at X must be durably committed **before** X returns the `TRANSFER_PROOF`. Therefore the source-side debit is final even if the inbound write at Y fails.

If Y's inbound write fails (disk, crash): consumer's plugin retries, providing the same proof; Y's tracker detects `ref = nonce` already absent in its chain and applies idempotently. If Y cannot apply for an extended period, the consumer's funds are stuck in a "transferred out of X, not yet in Y" state — a **federation-level issue** requiring operator intervention. Mitigation: transfer retry window of 24h with a final escalation to X to reverse.

### 4.4 Double-spend impossibility

Because X commits the outbound before issuing the proof, and a proof nonce is recorded, no second proof can be issued for the same nonce. Double-spend requires either (a) X's tracker equivocates (detected by Merkle roots) or (b) Y applies the same nonce twice (idempotent by `ref`).

## 5. Merkle-root archives

### 5.1 Collection

Each tracker maintains a `peer_root_archive` table (ledger spec §5.2): every `ROOT_ATTESTATION` it receives is persisted.

### 5.2 Verification

On receipt of a new `ROOT_ATTESTATION {tracker_id, hour, root, sig}`:

1. Verify `sig` against `tracker_id`'s known pubkey.
2. Check archive: have we already seen a `ROOT_ATTESTATION` for `(tracker_id, hour)` with a different `root`?
   - Yes → construct `EQUIVOCATION_EVIDENCE` and broadcast. Depeer `tracker_id`.
   - No → store and forward.

### 5.3 Long-term retention

Archives retained indefinitely (the whole point is historical verifiability). Grows ~24 × 32 bytes × number-of-peers / year per tracker. Negligible.

## 6. Revocation gossip

When a tracker freezes an identity (per reputation spec §4):

1. Tracker emits `REVOCATION {tracker_id, identity_id, reason, revoked_at, tracker_sig}`.
2. Neighbor trackers receive, persist to a revocation table, forward.
3. Each tracker applies the revocation to its registry: any connected session for `identity_id` is terminated; future `enroll` with the same Anthropic account fingerprint is refused.

### 6.1 Revocation scope

- Region of origin is authoritative for its own identities. A tracker in region X cannot revoke identities registered in Y; it can only locally refuse service.
- A malicious tracker can't mass-revoke — peers log repeated revocations from one source and alert operators if the rate spikes.
- False revocations are a known threat; **reputation-based freezes are local to the region that issued them** and are advisory elsewhere (peer trackers honor them by default but can operator-override).

## 7. Peer exchange & bootstrap

### 7.1 Peer exchange

Every hour, each tracker sends `PEER_EXCHANGE` to its peers, carrying a known-peers table:

```text
KnownPeer {
  tracker_id:   bytes32
  address:      url         // wss://tracker.example.org:443
  last_seen:    u64
  region_hint:  string      // human-friendly, e.g. "eu-central-1"
  health_score: float       // 0..1, observed by the reporter
}
```

Receivers merge into their known-peers database, deduping by `tracker_id`.

### 7.2 Bootstrap from plugin

Plugin's initial bootstrap list contains 3–5 trackers. On first successful connection, plugin fetches the tracker's top-N known-peers by `health_score`. From then on, the plugin can choose any tracker to connect to, ranked by latency.

### 7.3 Health score

Each tracker computes `health_score` for its peers based on:
- Uptime (time since last successful ROOT_ATTESTATION)
- Peering latency
- Revocation-gossip delay
- Equivocation incidents (disqualifying)

## 8. Trust model

- **Peer trackers are not trusted absolutely.** Every action carries a signature that can be independently verified.
- **A compromised tracker can:**
  - Equivocate its own ledger → exposed by Merkle-root archives at peers.
  - Issue false transfer proofs → detected by repeat/nonce checks and counterparty verification.
  - Refuse to honor legitimate transfer requests → consumers route around via a different home tracker.
  - Mass-revoke its own identities → scope-limited to its own region.
- **What a compromised tracker cannot do:**
  - Forge signatures for other trackers.
  - Modify remote trackers' ledgers.
  - Revoke identities in other regions.

## 9. Open questions

- **Gossip storm control.** A flood of revocations or root attestations could overwhelm small trackers. Token-bucket per-peer rate limits needed; values TBD.
- **Dynamic peering.** Currently operator-managed. Automatic peer discovery (like libp2p) would be friendlier but introduces sybil-peering concerns. Deferred.
- **Partial federation failures.** If 50% of peers become unreachable, transfers still work between online peers but reach fewer destinations. Need operator alerting, not protocol change.
- **Protocol versioning.** Minor-version-compatible changes OK via feature flags in HELLO; major-version changes require coordinated upgrade. Not an immediate issue.
- **Transfer reversal** (the 24h escalation in §4.3). Exact format and operator workflow TBD.

## 10. Acceptance criteria

- Two trackers can peer, exchange `ROOT_ATTESTATION`s, and archive each other's roots verifiably.
- A cross-region credit transfer works end-to-end, including the consumer's plugin seamlessly using the new balance at the destination.
- An intentionally-equivocating tracker is detected and depeered within one gossip interval.
- Revocations propagate to all reachable trackers within 5 minutes.
- Peer-exchange populates the plugin's known-peers table to ≥ 50 trackers within 1 hour of bootstrap in a network of that size.

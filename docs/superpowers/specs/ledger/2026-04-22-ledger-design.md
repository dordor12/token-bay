# Credit Ledger — Subsystem Design Spec

| Field | Value |
|---|---|
| Parent | [Token-Bay Architecture](../2026-04-22-token-bay-architecture-design.md) |
| Status | Design draft |
| Date | 2026-04-22 |
| Scope | The append-only signed log owned per-region by each tracker. Records every settled request. Defines on-disk format, indexing, queries, archival, equivocation detection. |

## 1. Purpose

Provide a tamper-evident, auditable record of every completed request settlement. The ledger is the source of truth for balance queries, reputation inputs, and cross-region credit transfers. Each tracker owns exactly one ledger for its region.

## 2. Interfaces

### 2.1 Exposed to tracker broker

- `append(entry)` → `entry_hash, seq` — append a finalized entry. Returns the entry's chain position.
- `balance(identity_id)` → `SignedBalanceSnapshot` — compute and sign a 10-minute-valid balance.
- `entry_by_hash(h)` → `Entry | NotFound`
- `entries_since(seq)` → stream of entries — used by federation and by audit/reputation.
- `current_root()` → `MerkleRoot` — current rolling Merkle root of all entries in this chain.

### 2.2 Exposed to federation

- `root_attestation(hour)` → `{hour, root, sig}` — hourly Merkle-root gossip (see federation spec).
- `begin_transfer_out(identity, amount, dest_region)` → `TransferProof` — writes outbound entry, returns a signed proof for the peer tracker.
- `apply_transfer_in(transfer_proof)` → `Entry` — verifies and records an inbound transfer from a peer.

### 2.3 Consumed

- Tracker signer (Ed25519 key).
- System clock (monotonic for `seq`; wall-clock for `timestamp`).
- Durable write-ahead log / durable kv store (choice deferred — see §7).

## 3. Data model

### 3.1 Entry (extends root spec §6.1)

```text
Entry {
  prev_hash:      bytes32    // SHA-256 of previous entry; zero for genesis
  seq:            u64        // strictly monotonic
  kind:           u8         // 0=usage, 1=transfer_out, 2=transfer_in, 3=starter_grant
  consumer_id:    bytes32    // Ed25519 pubkey hash; zero for transfer_out (self-paying) or starter_grant
  seeder_id:      bytes32    // Ed25519 pubkey hash; zero for non-usage kinds
  model:          string(32) // "" for non-usage kinds
  input_tokens:   u32        // 0 for non-usage kinds
  output_tokens:  u32        // 0 for non-usage kinds
  cost_credits:   u64        // credit delta (absolute value); sign determined by kind+party
  timestamp:      u64        // unix seconds
  request_id:     uuid       // zero UUID for non-usage kinds
  flags:          u32        // bit 0: consumer_sig_missing
  ref:            bytes32    // for transfer kinds: transfer UUID; else zero
  consumer_sig:   bytes64?   // nullable only if flags.consumer_sig_missing
  seeder_sig:     bytes64?   // present on usage entries only
  tracker_sig:    bytes64    // mandatory
}
```

All signatures are Ed25519 over the canonical serialization of the entry *excluding* the signature being computed.

### 3.2 Chain and Merkle root

- **Chain**: `entry[n].prev_hash = hash(entry[n-1])`. First entry has `prev_hash = 0` and a tracker-signed genesis note in a sidecar.
- **Merkle root**: every hour, tracker computes the Merkle root of all entries appended in that hour. Stored as `{hour, root, sig}` triples in a separate archival table. The Merkle tree is over entry hashes (already a chain, but Merkle provides log-n inclusion proofs).

### 3.3 Balance materialization

Balance is maintained as a live projection off the chain:

```text
Balance {
  identity_id:    bytes32
  credits:        i64        // signed; negative is allowed only within the starter grant band
  last_seq:       u64        // chain position of the last entry that touched this identity
  updated_at:     u64
}
```

Projection is updated inside the same durable transaction as `append(entry)`. On restart, the tracker replays any entries ahead of the projection's `last_seq`. Projection is a cache; the ledger chain is authoritative.

### 3.4 Signed balance snapshot

```text
SignedBalanceSnapshot {
  identity_id:   bytes32
  credits:       i64
  chain_tip_hash: bytes32
  chain_tip_seq:  u64
  issued_at:     u64
  expires_at:    u64        // issued_at + 600
  tracker_sig:   bytes64
}
```

Expiration is the freshness bound: beyond 10 minutes, the snapshot is invalid and consumers must refresh.

## 4. Algorithms

### 4.1 Append

1. Take the ledger write lock.
2. Read `tip_seq`, `tip_hash`.
3. Set `entry.prev_hash = tip_hash`, `entry.seq = tip_seq + 1`.
4. Validate entry: required sigs present per §3.1, counterparty signatures verify, `cost_credits` matches the model-weighted metering formula within 0.1% tolerance (seeder may round), `timestamp` within ±5 minutes of wall clock.
5. Verify balance update: `new_balance = old_balance + signed_delta`; abort if balance would go below `-starter_grant_band` (consumers cannot over-spend their grant).
6. In a single durable transaction: write entry, update both endpoints' balance projections, append to the Merkle-leaves table. Commit.
7. Release lock. Return `(hash(entry), entry.seq)`.

### 4.2 Balance query

1. Read `Balance{identity_id}` and `Chain{tip_hash, tip_seq}` in a read snapshot.
2. Assemble `SignedBalanceSnapshot` with `chain_tip_hash = tip_hash`, `chain_tip_seq = tip_seq`, `issued_at = now`, `expires_at = now + 600`.
3. Sign with tracker key. Return.

### 4.3 Merkle-root publication

Every hour, at a fixed cron:

1. Enumerate entries with `timestamp` in the past hour from the Merkle-leaves table.
2. Compute Merkle tree; root = `merkle_root`.
3. Persist `(hour, merkle_root, tracker_sig)` to the archival table.
4. Hand the triple to the federation subsystem for gossip.

### 4.4 Equivocation detection

Tracker equivocation = two different entries with the same `seq`, both signed by the same tracker. Detection runs when:

- A peer tracker's archived `{hour, root, sig}` gossip arrives with a conflicting root for an hour this tracker has already locally finalized.
- Two inbound `root_attestation` messages from the same tracker for the same hour show different roots.

On detection: emit `EquivocationEvidence{tracker_id, hour, root_a, sig_a, root_b, sig_b}`. Evidence is broadcast via federation gossip. Any tracker holding this evidence depeers the equivocator within one heartbeat interval.

## 5. Storage backend

### 5.1 Choice (deferred; candidates)

- **SQLite WAL mode** — single-node tracker, simple, proven, WAL provides durability.
- **PostgreSQL** — same model, scales better under contention, overkill for small trackers.
- **RocksDB / LMDB with manual schema** — fast, but we'd reinvent relational queries we need.

Leaning **SQLite WAL** for v1 because regional trackers are expected to handle 10²–10³ writes/sec in steady state, and SQLite handles that comfortably. Revisit if scale demands.

### 5.2 Schema sketch (SQL-ish, implementation-agnostic)

```sql
CREATE TABLE entries (
  seq         INTEGER PRIMARY KEY,          -- monotonic
  prev_hash   BLOB(32) NOT NULL,
  hash        BLOB(32) NOT NULL UNIQUE,
  kind        INT NOT NULL,
  consumer_id BLOB(32),
  seeder_id   BLOB(32),
  model       TEXT,
  input_tok   INT,
  output_tok  INT,
  cost_credit INT NOT NULL,
  timestamp   INT NOT NULL,
  request_id  BLOB(16),
  flags       INT NOT NULL,
  ref         BLOB(32),
  consumer_sig BLOB(64),
  seeder_sig   BLOB(64),
  tracker_sig  BLOB(64) NOT NULL,
  canonical    BLOB NOT NULL                -- canonical serialization
);

CREATE INDEX idx_entries_consumer ON entries(consumer_id, seq);
CREATE INDEX idx_entries_seeder   ON entries(seeder_id, seq);
CREATE INDEX idx_entries_time     ON entries(timestamp);

CREATE TABLE balances (
  identity_id BLOB(32) PRIMARY KEY,
  credits     INT NOT NULL,
  last_seq    INT NOT NULL,
  updated_at  INT NOT NULL
);

CREATE TABLE merkle_roots (
  hour        INT PRIMARY KEY,
  root        BLOB(32) NOT NULL,
  tracker_sig BLOB(64) NOT NULL
);

CREATE TABLE peer_root_archive (      -- peer trackers' root gossip
  tracker_id  BLOB(32),
  hour        INT,
  root        BLOB(32) NOT NULL,
  sig         BLOB(64) NOT NULL,
  received_at INT NOT NULL,
  PRIMARY KEY (tracker_id, hour)
);
```

## 6. Failure handling

| Failure | Behavior |
|---|---|
| Partial write (crash mid-transaction) | SQLite WAL atomic commit; entry either visible or not. Restart replays WAL. |
| Projection out of sync after restart | Recompute from `entries` where `seq > balances.last_seq`. Idempotent. |
| Tracker key compromise | Out of scope for this subsystem; handled at tracker service level. Ledger continues with new tracker key after re-signing the chain tip under the new key. |
| Peer claims equivocation falsely | Evidence format requires two valid signatures from this tracker for the same hour. If evidence doesn't verify, peer is ignored. |
| Disk full | Append returns error; tracker broker must fail incoming settlements gracefully (error propagates back to consumer; no credit moved). |
| Clock skew | `timestamp` within ±5 min of tracker wall clock, else append rejects. Tracker should run NTP. |

## 7. Open questions / future work

- **Storage backend benchmark** — pick between SQLite, Postgres, and embedded RocksDB for v1. Rough benchmark harness needed.
- **Archival and pruning** — entries never delete; ledger grows unboundedly. Plan for periodic archival snapshots (bundled Merkle-rooted archives) that older records can be offloaded to, with tip-only live queries. Not needed for v1.
- **Batch signing** — high-throughput trackers may want to batch-sign entries. Requires rethinking the `tracker_sig` over the entry hash to be signed over a Merkle leaf instead. Deferred.
- **Balance quantization** — credits are u64 with an implicit divisor (e.g., 10⁶ so 1.0 credit = 1000000). Divisor choice deferred; affects all cost formulas.

## 8. Acceptance criteria

- All entries in the chain verify: `hash(entry[n-1]) == entry[n].prev_hash` ∀ n.
- All entries have valid tracker and seeder signatures; consumer sig valid when present.
- Balance projection matches chain replay for every identity.
- Hourly Merkle roots signed and archived.
- Balance snapshot signatures verify under tracker pubkey; snapshots expire per `expires_at`.
- Equivocation evidence reliably triggers depeering within one heartbeat.

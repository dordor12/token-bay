# Regional Tracker — Subsystem Design Spec

| Field | Value |
|---|---|
| Parent | [Token-Bay Architecture](../2026-04-22-token-bay-architecture-design.md) |
| Status | Design draft |
| Date | 2026-04-22 |
| Scope | The tracker is the coordination server for a region. Owns the live seeder registry, the request broker, the regional credit ledger (see ledger spec), and NAT rendezvous. This spec covers the service's public RPCs, selection algorithm, broker concurrency model, and STUN/TURN operation. |

## 1. Purpose

Be the control-plane authority for a region. Consumers submit request envelopes; the tracker picks a capable seeder, coordinates the handshake, relays data if NAT traversal fails, and finalizes ledger entries.

The tracker is **not** in the data path by default — it's a rendezvous and broker. Data path falls back through the tracker only when hole-punching fails.

## 2. Interfaces

### 2.1 Exposed to plugins (long-lived connection)

Connection is a single mutually-authenticated TLS (or QUIC) stream. Multiplexed logical channels:

- `enroll(identity_proof)` → `{identity_id, starter_grant_entry}`
- `heartbeat()` → bidirectional keepalive + availability updates
- `advertise(capabilities, available, headroom)` — seeder side
- `broker_request(envelope)` → `seeder_assignment | NO_CAPACITY`
- `offer(envelope_hash, terms)` → seeder replies `accept(ephemeral_pubkey) | reject(reason)`
- `usage_report(request_id, counts, seeder_sig)` — seeder after request completes
- `settle_request(entry_preimage)` → consumer counter-signs → `settle_ack`
- `balance(identity_id)` → `SignedBalanceSnapshot`
- `transfer_request(identity_id, amount, dest_region)` → `TransferProof`
- `stun_allocate()` → public reflexive address for hole-punching
- `turn_relay_open(session_id)` → allocates a relay channel when hole-punch fails

### 2.2 Exposed to peer trackers (federation)

See federation spec. Trackers also speak the federation protocol over a separate long-lived link.

### 2.3 Consumed

- Ledger subsystem (append, balance, roots).
- Reputation subsystem (score lookup, audit-mode state, freezes).
- Exhaustion-proof validator (L2 when enabled).
- Federation subsystem (Merkle-root gossip, cross-region transfers, revocation gossip).

## 3. Internal modules

```
tracker/
  listener/        -- TCP/QUIC accept loop; TLS termination
  session/         -- per-connection state machine (plugin or peer)
  registry/        -- live seeder index (sharded)
  broker/          -- selection + assignment logic
  ledger-client/   -- calls into the ledger subsystem
  reputation-client/
  federation/      -- peer-tracker client + server
  stun/            -- STUN-like reflexive address service
  turn/            -- relay fallback
  metrics/         -- counters, histograms, health endpoint
  admin-api/       -- operator endpoints (stats, peering mgmt)
```

## 4. Data structures

### 4.1 Seeder registry entry (in-memory)

```text
SeederRecord {
  identity_id:       bytes32
  conn_session_id:   u64             // pointer into session module
  capabilities: {
    models:          [string]        // e.g. ["claude-opus-4-7", "claude-sonnet-4-6"]
    max_context:     u32
    tiers:           [standard, tee_attested?]
    attestation:     EnclaveAttestation?   // present iff tier includes tee_attested
  }
  availability:      bool
  headroom_estimate: float            // 0.0 to 1.0, from seeder's advertise
  reputation_score:  float            // from reputation subsystem
  net_coords: {
    external_addr:   ip:port          // from STUN
    local_candidates: [ip:port]
  }
  load:              int              // currently serving offers
  last_heartbeat:    u64
}
```

Registry is sharded by `hash(identity_id) % num_shards` to avoid hot locks.

### 4.2 In-flight request state

```text
InflightRequest {
  request_id:       uuid
  consumer_id:      bytes32
  envelope:         Envelope
  max_cost_reserved: u64
  assigned_seeder:  bytes32?
  offer_attempts:   [bytes32]       // seeders tried in order
  started_at:       u64
  state:            SELECTING | ASSIGNED | SERVING | COMPLETED | FAILED
}
```

Expires and is GC'd 10 minutes after `state = COMPLETED | FAILED`.

## 5. Algorithms

### 5.1 Broker selection

Entry: `broker_request(envelope)`.

1. Validate envelope: consumer signature, balance snapshot freshness, exhaustion proof (v1 two-signal bundle — `stop_failure` + `usage_probe`; see exhaustion-proof spec §3). v2 Claude-Code-attested form when available. Reject with specific error codes.
2. Compute `max_cost` from envelope. Verify `balance_snapshot.credits >= max_cost`. Reserve `max_cost` in an in-memory reservation ledger (decremented from the consumer's working balance; released on completion or timeout).
3. Query registry for candidate seeders where:
   - `availability == true`
   - `capabilities.models` contains `envelope.model`
   - `capabilities.tiers` satisfies `envelope.tier`
   - `headroom_estimate >= θ_headroom` (default 0.2)
   - `load < θ_load` (default 5 concurrent)
   - reputation not frozen
4. Score each candidate:
   ```
   score = α · reputation_score
         + β · headroom_estimate
         − γ · estimated_rtt_to_consumer
         − δ · load
   ```
   Defaults: `α=0.4, β=0.3, γ=0.2, δ=0.1`. Tunable per region.
5. Pick top candidate; send `offer(envelope_hash, terms)`. Mark seeder's load +1.
6. Seeder replies `accept` or `reject(reason)` within `offer_timeout_ms` (default 1500ms).
7. On reject: log reason for reputation, try next candidate. Cap at `max_offer_attempts` (default 4) before returning `NO_CAPACITY` to consumer.
8. On accept: return `seeder_assignment {seeder_addr, seeder_pubkey, reservation_token}` to consumer. Transition `InflightRequest.state = ASSIGNED`.

### 5.2 Settlement

Entry: `usage_report(request_id, input_tokens, output_tokens, model, seeder_sig)` from seeder.

1. Look up `InflightRequest`. Validate state, seeder identity match, model match.
2. Compute `actual_cost = input_tokens · in_rate[model] + output_tokens · out_rate[model]`.
3. Validate `actual_cost <= max_cost_reserved` (+ 5% tolerance for last-token rounding). Reject if over.
4. Build entry preimage (all fields from ledger §3.1 except the three signatures). Seeder's `seeder_sig` is the seeder's signature over this preimage; verify it.
5. Send `settle_request(preimage)` to consumer. Await `consumer_sig` with timeout `settlement_timeout_s` (default 900 = 15 min).
6. On consumer_sig received: verify, build full entry, sign with tracker key, append to ledger.
7. On timeout: build entry with `flags.consumer_sig_missing = true`, consumer_sig = NULL; still append. Consumer can dispute later.
8. Release reserved credits; record actual debit/credit in ledger (append is atomic with balance update).

### 5.3 Timeouts

| Timeout | Default | Action on expiry |
|---|---|---|
| `offer_timeout_ms` | 1500ms | Treat as `reject(timeout)`, try next candidate |
| `tunnel_setup_ms` | 10s | Consumer reports failure; tracker releases reservation |
| `stream_idle_s` | 60s | No data flowing = stream broken; fail request |
| `settlement_timeout_s` | 900s | Finalize with `consumer_sig_missing` |
| `reservation_ttl_s` | 1200s | Hard release if above timeouts fail |

### 5.4 STUN / TURN

- **STUN (binding request)**: trivial reflexive-address reflection. On each heartbeat from a seeder, tracker records `external_addr`. Consumers also get their own reflexive address at request time.
- **TURN-like relay**: lazy-allocated per `request_id` on fallback. Tracker exposes a UDP relay endpoint, both sides connect to it with a session token. Rate-limited per seeder to prevent bandwidth abuse. Relay costs are tracked as a future bandwidth-credit concept (see root spec §9).

## 6. Concurrency model

- One async runtime (tokio/async-std-style). Per-connection state machine runs on its own task.
- Registry shards are lock-free (DashMap-style) or use fine-grained locks per shard.
- Ledger writes are serialized through the ledger subsystem's own lock (§4.1 of ledger spec); the tracker surfaces async wrappers.
- Broker is CPU-light (scoring a few candidates); most latency is network round-trips.

Expected capacity per modest tracker node (2 vCPU, 4GB RAM): 10³ concurrent plugins, 10² broker_requests/sec. Scaling via horizontal tracker replicas with shared ledger is a v2 concern.

## 7. Failure handling

| Failure | Behavior |
|---|---|
| Plugin disconnects mid-request | Fail `InflightRequest`. Release reservation. If data path was direct, seeder sees tunnel close and drops. If tunnel relayed via TURN, tracker tears down. |
| Seeder disconnects after accepting offer | Fail request. Broker does not retry automatically (consumer retries from CLI per Q7c(i)). |
| Seeder disconnects after `usage_report` but before settlement | Entry still finalizes; seeder_sig was already provided on the report. |
| Consumer disconnects after response stream complete but before `settle_ack` | Finalize with `consumer_sig_missing` after timeout (§5.3). |
| Ledger full / disk error | `500` to broker request; broker cannot accept new work until recovered. Existing in-flight requests fail. Alert operator. |
| Reputation subsystem unreachable | Degrade gracefully: score defaults to 0.5 for all candidates; freeze list cached for 10 min. |
| Federation unreachable | Local broker still works. Cross-region transfers queue. Revocation gossip delays. |
| Invalid exhaustion proof | Reject broker_request with `EXHAUSTION_PROOF_INVALID`. Record for reputation. |

## 8. Operator interface

Admin HTTP API on a separate internal port:

- `GET /health` — liveness + version
- `GET /stats` — counters (connections, broker reqs/sec, ledger tip seq, merkle_root_hour)
- `GET /peers` — federation peer list with link health
- `POST /peers/add` → add a peer tracker (operator action)
- `POST /peers/remove` → deliberate depeering
- `GET /identity/<id>` — debug inspect (reputation score, balance, last activity)
- `POST /maintenance` → graceful shutdown (reject new connections, drain in-flight)

## 9. Security model

- **TLS everywhere.** Tracker exposes only TLS on public ports.
- **Identity auth.** Every RPC from a plugin or peer is on a connection whose identity was proven at handshake.
- **Rate limiting.** Per-identity rate limits on `broker_request` (default 2/sec). Per-IP limits on `enroll` (default 1/min/IP).
- **Bootstrap list signing.** Tracker's public cert + identity are included in the plugin's distributed bootstrap list, signed by the project release key. Prevents first-contact MITM.

## 10. Open questions

- **Horizontal scaling.** v1 single-process. If a region grows past one-tracker capacity, we need either sharding (by identity_id) or a leader-elected replica set. Design deferred.
- **Turn relay bandwidth fairness.** TURN relay costs real bandwidth; need a credit scheme for operators running trackers. Probably ties into "bandwidth-as-credit" from root §9.
- **Region migration.** Consumer moves geographically; how do they switch trackers smoothly without losing the starter grant or reputation? Current answer: transfer their credits via federation; reputation is region-local for v1. Revisit.
- **Broker selection fairness.** Pure score-based selection favors top seeders disproportionately. Consider ε-greedy or weighted-random pick over top-k to spread load and grow new seeders' reputation.

## 11. Acceptance criteria

- Tracker serves ≥ 100 concurrent plugin connections on a modest VM.
- `broker_request` end-to-end latency < 50ms p50, < 200ms p99 when a suitable seeder exists.
- Ledger settlements always have valid tracker signatures; untimely consumer signatures produce `consumer_sig_missing` entries (never lost entries).
- STUN hole-punching succeeds on ≥ 80% of consumer-seeder pairs with common home NATs. TURN fallback on the remainder.
- Graceful shutdown drains in-flight requests without data loss.

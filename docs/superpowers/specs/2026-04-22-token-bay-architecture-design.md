# Token-Bay — Architecture Design

| Field | Value |
|---|---|
| Status | Design draft |
| Date | 2026-04-22 |
| Scope | Top-level architecture overview; each subsystem gets its own child spec |
| Intent | **Educational / research exercise in distributed-systems design.** Not intended for deployment against live Anthropic accounts. Credential-sharing across users would violate Anthropic's Terms of Service; this document explores the *protocol patterns* (federated trackers, signed reputation, NAT-traversed P2P data paths) using Claude rate-limit capacity as the notional payload. |

## 0. Design-decision reference

The following choices were resolved during brainstorming and are load-bearing across every section. Change one and the spec changes with it.

| # | Decision | Choice |
|---|---|---|
| Q1 | What moves over the wire | **Seeder-proxied request.** Consumer's prompt goes to the seeder; seeder calls Anthropic with its own token; response streams back. Consumer never holds the seeder's token. |
| Q2 | Participation & trust model | **Open public network.** Anyone can join with a pseudonymous identity. No curation. |
| Q3 | Privacy & abuse propagation | **Layered:** plaintext-with-audit-signatures baseline + TEE-attested opt-in tier for sensitive work. |
| Q4 | Credit metering | **Per-token (input + output) weighted by model.** Credits track Anthropic's own accounting. |
| Q5 | Identity / sybil resistance | **Proof-of-Anthropic-account**, mediated via the Claude Code bridge. Identity = Ed25519 keypair; account binding relies on a Claude-Code-surfaced signal (see [plugin spec §4.2](./plugin/2026-04-22-plugin-design.md)). v1 default is observational (`β`); long-term target is a Claude-Code-issued signed primitive (`α`). |
| Q6 | Credit ledger | **Per-tracker append-only signed log** (federated), hash-chained entries, hourly Merkle-root commits gossiped to peer trackers. |
| Q7a | Cold start | **Starter grant:** 50 credits on first verified enrollment; non-refillable. |
| Q7b | Idle window | **Hybrid:** user picks scheduled time-window mode *or* always-on mode at enrollment. |
| Q7c | Mid-stream failure | **Fail-and-surface.** Error propagates to the consumer's CLI; no silent auto-reroute. |
| Add. | Rate-limit exhaustion gate | Claude Code itself makes the first attempt. Network fallback engages only on Claude Code's `StopFailure{matcher: rate_limit}` hook **and** a confirming `claude -p "/usage"` probe (two-stage gate). Three layers of validation: client-side two-stage gate → cryptographic exhaustion proof → reputation-based backstop. |

---

## 1. System overview

### 1.1 The five subsystems

1. **Consumer plugin** — A Claude Code plugin. Registers a `StopFailure{rate_limit}` hook, slash commands (`/token-bay fallback`, `/token-bay status`, etc.), and a background sidecar that maintains the tracker connection. In normal operation it does **not** intercept Claude Code's HTTPS traffic — Claude Code talks to Anthropic directly using its own authentication. When a turn fails with a rate-limit error, the plugin verifies via `claude -p "/usage"`, asks the user to consent, then redirects Claude Code's next `/v1/messages` call **mid-session** to the sidecar by atomically editing `~/.claude/settings.json` (setting `ANTHROPIC_BASE_URL=http://127.0.0.1:PORT`). Claude Code's native settings watcher propagates the change within ~50ms; the next request arrives at the sidecar's Anthropic-compatible `ccproxy` endpoint, which routes through the Token-Bay network. No Claude Code restart. Details in the [plugin spec §2.5](./plugin/2026-04-22-plugin-design.md#25-mid-session-redirect-mechanism).
2. **Seeder plugin** — Same plugin binary, different role. Registers with a regional tracker, advertises availability when the user's idle policy permits, accepts forwarded requests, and serves them via `claude -p "<prompt>"` with tool-disabling flags (no `Bash`, `Read`, `Write`, MCP, or hooks). Claude Code authenticates and talks to Anthropic on its own; the seeder plugin never holds credentials. Response streams back through `claude -p` stdout and over the tunnel.
3. **Regional tracker** — A server that holds a live seeder registry, brokers requests, owns the region's credit ledger, and serves as NAT rendezvous for peer-to-peer tunnels. One tracker serves a latency-defined region; regions may overlap physically.
4. **Federation layer** — The protocol by which regional trackers peer with each other. Carries Merkle-root attestations, cross-region credit-transfer proofs, and identity-revocation gossip.
5. **Credit ledger** — The append-only, hash-chained, signed log of completed request settlements, owned per-region by each tracker.

### 1.2 Topology (text form)

```
  ┌─────────────────────┐                                ┌──────────────────────┐
  │ Consumer host       │                                │ Seeder host (idle)   │
  │                     │                                │                      │
  │  Claude Code CLI    │       (long-lived TLS)         │  Seeder plugin       │
  │        │            │                                │        │             │
  │  Consumer plugin ◄──┼────►  Regional tracker  ◄──────┼───  (registry,       │
  │  (hooks + sidecar   │      (registry, broker,        │     heartbeat)       │
  │   + ccproxy)        │       ledger, STUN)            │                      │
  └──────────┬──────────┘               ▲                └──────────┬───────────┘
             │                          │                           │
             │                          ▼                           │
             │             ┌──────────────────────┐                 │
             │             │ Peer trackers        │                 │
             │             │ (other regions)      │                 │
             │             │ Merkle-root gossip + │                 │
             │             │ credit-transfer      │                 │
             │             │ proofs               │                 │
             │             └──────────────────────┘                 │
             │                                                      │
             │                                                      ▼
             │          per-request flow (dashed in §2):   ┌────────────────────┐
             └──────────── P2P encrypted tunnel ─────────► │ api.anthropic.com  │
                        (NAT hole-punch via tracker,       └────────────────────┘
                         tracker-relayed fallback)
```

### 1.3 Data-path choice

Per-request traffic attempts peer-to-peer (consumer plugin ↔ seeder plugin) first, using the tracker as a STUN-like rendezvous for NAT hole-punching. If hole-punching fails (e.g., symmetric NAT on both sides), the tracker falls back to relaying the data path. Control plane (broker handshake, credit reservation, ledger settlement) always traverses the tracker.

---

## 2. Request lifecycle

### 2.1 Happy path

| Step | Actor | Action |
|---|---|---|
| 1 | Claude Code CLI → `api.anthropic.com` | Claude Code makes `POST /v1/messages` directly, using its own authentication. Plugin does not intercept. |
| 2 | Anthropic → Claude Code | **`2xx`** → response streams to the CLI; plugin uninvolved. **`429`** → Claude Code ends the turn with `StopFailure{matcher: rate_limit}`. |
| 3 | `StopFailure` hook → Consumer plugin | Plugin captures the failure payload (error shape, timestamp) into a pending-fallback ticket. |
| 3a | Consumer plugin | Runs `claude -p "/usage"` with tool-disabling flags to independently verify rate-limit exhaustion (two-stage gate). Appends the `/usage` output to the ticket. |
| 3b | Consumer plugin → User | Offers network fallback (cost estimate + current balance). On user decline, stop. |
| 3c | Consumer plugin (sidecar) | On user consent: atomically rewrites `~/.claude/settings.json` to set `env.ANTHROPIC_BASE_URL=http://127.0.0.1:PORT`. Records rollback. Claude Code's settings file watcher picks up the change; its next `/v1/messages` call will route to the sidecar. (Plugin spec §2.5 + §5.3.) |
| 3d | User | Types next prompt. Claude Code builds a fresh Anthropic client that reads the new `ANTHROPIC_BASE_URL`. |
| 4 | Claude Code → Consumer plugin (`ccproxy`) | POST `/v1/messages` arrives at sidecar. Sidecar assembles envelope with `exhaustion_proof = {stop_failure, usage_probe}` from the cached ticket; signs. |
| 4a | Consumer plugin → Tracker | `broker_request(model, max_input_tokens, max_output_tokens, tier, body_hash, exhaustion_proof, consumer_sig, balance_proof)` |
| 5 | Tracker | Select a seeder. Scoring: `score = reputation × α + headroom × β − latency × γ − load × δ`. Tier filter applied first. |
| 6 | Tracker → Seeder | `offer(consumer_id, envelope_hash, terms)` |
| 7 | Seeder → Tracker | `accept(ephemeral_pubkey)` or `reject(reason)` — on reject, tracker tries next candidate. |
| 8 | Tracker → Consumer plugin | `seeder_addr, seeder_pubkey, reservation_token` |
| 9 | Consumer ↔ Seeder | NAT hole-punch attempt via tracker's STUN; fallback to tracker-relayed tunnel. |
| 10 | Consumer plugin → Seeder plugin | Request body over the tunnel (TLS). Standard tier: seeder plugin process decrypts. |
| 11 | Seeder plugin | Invokes `claude -p "<prompt>"` with tool-disabling flags (`--disallowedTools "*" --mcp-config /dev/null`, empty hooks). |
| 12 | Claude Code ↔ `api.anthropic.com` (seeder side) | Seeder's Claude Code sends `POST /v1/messages` under its own auth; Anthropic returns a streaming SSE response. |
| 13 | `claude -p` stdout → Seeder plugin → Consumer plugin | Seeder plugin reads stream-json chunks from `claude -p` and relays over the tunnel. |
| 14 | Consumer plugin (`ccproxy`) → Claude Code | Stream SSE chunks back over the HTTP response socket — this is the same socket Claude Code opened in step 4. Claude Code renders the streaming output as a normal turn. |
| 15 | Seeder → Tracker | `usage_report(input_tokens, output_tokens, model, seeder_sig)` |
| 16 | Tracker | Compute `cost_credits` from the weighted metering function. Construct ledger entry. Sign. |
| 17 | Tracker → Consumer plugin | `settlement_request(entry_preimage)` |
| 18 | Consumer plugin → Tracker | `consumer_sig` on the entry. Tracker finalizes entry into its chain. Consumer balance debited; seeder balance credited. |

### 2.2 Invariants

- **Tracker never sees prompt plaintext.** It sees only `body_hash` in the envelope. Selection and billing do not need content access.
- **Credit reservation is optimistic.** Step 3 reserves the upper bound; step 16 debits the real cost. Any excess is released.
- **Up to three signatures per ledger entry** (consumer, seeder, tracker). Seeder and tracker signatures are mandatory; consumer signature may be absent if the consumer became unreachable at settlement — in that case the entry is finalized with a `consumer_sig_missing` flag and is subject to dispute (see §8). Invalid (as opposed to missing) signatures always cause rejection.
- **Two-signal exhaustion proof.** Consumer cannot fake a rate-limit condition without fabricating both a plausible `StopFailure{rate_limit}` payload and a plausible `/usage` output — two signals, both under consumer signature, each sourced from the user's own Claude Code. Seeder cannot charge a consumer who was not exhausted.

---

## 3. Tracker & federation

### 3.1 Bootstrap & discovery

- **Hard-coded bootstrap list** — The plugin binary ships with 3–5 well-known bootstrap tracker addresses, signed with the project's release key. An attacker with a compromised mirror cannot swap the list silently.
- **User override** — Config accepts an explicit `tracker: <url>` which bypasses the bootstrap list. This is how a private/community tracker is used.
- **Peer exchange after first contact** — Once connected, the plugin pulls the tracker's peer list (carried by the federation layer alongside Merkle-root gossip). From then on, the plugin has hundreds of known trackers and chooses by ping latency.

Bootstrap trust is narrow: it only matters on first join, and a new identity has nothing to steal (zero credits other than the starter grant; Anthropic-challenge auth still holds).

### 3.2 Tracker responsibilities

- **Seeder registry.** Live records of connected seeders: `(identity_id, capabilities, headroom, reputation, net_coords)`. Updated via heartbeat.
- **Request broker.** Applies the selection function from §2.1 step 5.
- **Regional credit ledger.** See §6.
- **NAT rendezvous.** STUN-style hole-punching coordination; data-path relay fallback.

### 3.3 Region definition

Latency-based, not political. A node's "region" is whichever tracker it can reach within sub-50ms RTT. Trackers self-declare geographic hints for UX only. A single physical region may host multiple trackers for redundancy.

### 3.4 Federation protocol

Trackers peer pairwise over long-lived TLS links. Each link carries:

- **Merkle-root attestations** (hourly) — `{tracker_id, ledger_root, seq_tip, t, tracker_sig}`. Lets peers archive a verifiable history; any later rewrite is exposed by comparison.
- **Cross-region credit transfers** — when a consumer in region X requests credits earned in region Y, the plugin fetches a signed transfer proof from Y: `{identity, credits, source_chain_tip_hash, dest_region, nonce, Y_tracker_sig}`. X adds a matching inbound entry referencing the same nonce; Y adds the outbound entry in its chain before granting the proof. Double-spend is prevented by the ordering invariant (Y's outbound entry predates X's inbound entry).
- **Identity revocation list gossip** — frozen identities (per §7.3) propagate federation-wide in minutes.

### 3.5 Trust model

Federated, not trustless. You trust your tracker; your tracker vouches for its peers via signed peering. A malicious tracker can rewrite its own recent ledger but archived Merkle roots at peer trackers expose the rewrite; cross-region transfers cease to be honored once a tracker is distrusted. Bootstrap list is the root of trust.

---

## 4. Seeder plugin

### 4.1 Lifecycle

1. **Enroll.** User runs `/token-bay enroll` (or `token-bay enroll --role seeder`). Plugin generates an Ed25519 keypair, verifies Claude Code account binding via the bridge ([plugin spec §4.2](./plugin/2026-04-22-plugin-design.md)), registers identity with the tracker. **No Anthropic API key is handled by the plugin at any point.** Starter grant of 50 credits applied (Q7a(i)).
2. **Connect.** Plugin opens a long-lived, mutually authenticated TLS connection to the regional tracker. Heartbeat every 30s.
3. **Advertise availability.** Plugin sends `available=true` iff **all** of:
   - User's idle policy permits: `mode=scheduled` → current local time ∈ configured window; `mode=always_on` → always true.
   - No active Claude Code session locally in the last `activity_grace` minutes (default 10m).
   - Rate-limit headroom ≥ threshold: no recent `StopFailure{rate_limit}` signals from the user's local Claude Code sessions; optionally re-probed with `claude -p "/usage"`. Conservative — the plugin has no direct view of the user's quota.
4. **Serve.** Receive `offer` from tracker → decide to accept based on capabilities and current load → open tunnel to consumer → invoke `claude -p "<prompt>"` with tool-disabling flags ([plugin spec §6.2](./plugin/2026-04-22-plugin-design.md)) → stream response back over tunnel → report usage to tracker (§2.1 step 15).
5. **Withdraw.** On user activity (keypress, Claude Code session start), plugin immediately sends `available=false`. In-flight requests complete; no new offers accepted.

### 4.2 Non-persistence of content

Seeder plugin **does not persist** prompt or response content at any time. Memory is zeroed on stream completion.

### 4.3 Audit log (mandatory)

Seeder keeps a local, append-only audit log: `(request_id, model, input_tokens, output_tokens, consumer_id_hash, started_at, completed_at)`. No content. The audit log is the seeder's defense if the Anthropic account is later flagged — it proves "I forwarded this request at this time for this consumer" and can be cross-referenced against the tracker's ledger entry. Audit log retention: minimum 90 days.

### 4.4 Seeder-side headroom guard

Before accepting an offer, seeder plugin verifies its own rate-limit headroom by checking recent `StopFailure{rate_limit}` activity on the user's local Claude Code sessions, optionally re-probed with `claude -p "/usage"`. Refuses offers that would push it past its own limit. Protects the seeder's Claude Code account from being exhausted unexpectedly while the user might want to use it.

---

## 5. Consumer plugin

### 5.1 Per-request flow (inside the plugin)

1. Claude Code makes an API call to Anthropic with its own authentication. Plugin does not intercept.
2. On `2xx`: plugin not involved. Response reaches the user's CLI normally.
3. On `429`: Claude Code's turn ends with `StopFailure{matcher: rate_limit}`. Plugin's hook fires, capturing the failure payload.
4. Plugin invokes `claude -p "/usage"` with tool-disabling flags as an independent second signal confirming rate-limit state (two-stage gate).
5. Plugin offers the user a fallback confirmation (cost estimate + balance).
6. On user consent: build envelope with `exhaustion_proof = {stop_failure, usage_probe}`, sign, send to tracker.
7. Tracker assigns a seeder; plugin opens tunnel (NAT hole-punch → tracker-relay fallback).
8. Send encrypted conversation body over the tunnel; receive streamed response.
9. Inject streamed response back into the Claude Code conversation (exact mechanism per [plugin spec §10](./plugin/2026-04-22-plugin-design.md) open question).
10. On stream end: counter-sign the tracker's settlement request.
11. On stream error mid-flight (per Q7c(i)): surface the error; no credit debited; no entry written.

### 5.2 Configuration (`~/.token-bay/config.yaml`)

```yaml
role: both               # consumer | seeder | both
tracker: auto            # auto (bootstrap) or URL
# NOTE: no API-key config — the plugin uses the Claude Code CLI bridge
# and never holds an Anthropic token (see plugin spec §6.2)
idle_policy:
  mode: scheduled        # scheduled | always_on
  window: "02:00-06:00"  # local tz; only if scheduled
  activity_grace: 10m    # delay after last Claude Code session before re-advertising
privacy_tier: standard   # standard | tee_required | tee_preferred
max_spend_per_hour: 500  # consumer throttle (credit ceiling)
audit_log_path: ~/.token-bay/audit.log
```

### 5.3 Rate-limit exhaustion gate

Implemented as three layers:

- **L1 — Client-side two-stage gate.** Plugin is driven by Claude Code's own `StopFailure{rate_limit}` hook and independently re-verifies via `claude -p "/usage"` before spending any credits (§5.1 steps 3–4). The reference implementation is open-source (licensing TBD — assumed permissive, e.g. Apache-2.0 or MIT); L1 behavior is auditable by inspecting the reference build. A forked plugin can still fabricate both signals — that is the threat L3 exists to catch, with L2 closing it cryptographically.
- **L2 — Cryptographic proof.** See §7.2 for the v1 (self-attested) and v2 (zkTLS / TLSNotary MPC) forms.
- **L3 — Reputation-based backstop.** Statistical outliers in exhaustion-claim rate trigger audit mode; repeat offenders get identity-frozen. See §7.3.

### 5.4 Cold-start behavior

On first enrollment: starter grant of 50 credits applied to the consumer's balance in their regional ledger. One grant per Anthropic-account-backed identity (Q7a(i)). Non-refillable — once spent, user must earn credits by seeding to consume further.

---

## 6. Credit ledger

### 6.1 Entry format

```text
Entry {
  prev_hash:      bytes32       (SHA-256 of previous entry in this tracker's chain; zero for genesis)
  seq:            u64           (monotonic)
  consumer_id:    bytes32       (Ed25519 pubkey hash)
  seeder_id:      bytes32       (Ed25519 pubkey hash)
  model:          string        ("claude-opus-4-7" etc.)
  input_tokens:   u32
  output_tokens:  u32
  cost_credits:   u64           (from the model-weighted metering function)
  timestamp:      u64           (unix seconds)
  request_id:     uuid v4       (cross-reference to seeder's audit log)
  flags:          u32           (bit 0: consumer_sig_missing; other bits reserved)
  consumer_sig:   bytes64?      (Ed25519 over preceding fields; NULL allowed iff flags.consumer_sig_missing)
  seeder_sig:     bytes64       (Ed25519 over same preimage; mandatory)
  tracker_sig:    bytes64       (Ed25519 over preimage + present sigs; mandatory)
}
```

Entry hash chains via `prev_hash`. Tracker publishes `merkle_root_{hour}` of its current chain every hour.

### 6.2 Merkle-root history

Peer trackers archive each other's `(hour, merkle_root, tracker_sig)` triples indefinitely. To rewrite past history, a tracker would need to re-sign archived roots held by every peer — infeasible once gossip propagates.

### 6.3 Balance query

`balance(identity_id)` → returns `(balance, chain_tip_hash, tracker_sig, expires_at)`. Signed snapshot valid for 10 minutes; longer-lived cached balance violates freshness, consumer plugin refreshes.

### 6.4 Cross-region transfers

As in §3.4. Outbound entry at source tracker, inbound entry at destination tracker, shared `transfer_uuid`, mutual reference. Transfer is atomic by ordering: outbound precedes inbound in wall-clock and each chain.

### 6.5 Equivocation detection

Tracker signing two conflicting chain tips (different entries at the same `seq`): detected by any peer holding a prior archived root. Triggers federation alert, automatic depeering, consumer migration workflow.

---

## 7. Privacy & abuse controls

### 7.1 Tier model

Per Q3(d):

- **Standard tier (default).** Plaintext proxy. Seeder sees request body inside its own process. Audit log + three-signature ledger entry provide **non-repudiation**, not **confidentiality**. Consumers must treat standard-tier requests as visible to a random seeder.
- **TEE-attested tier.** Seeder runs proxy inside an SGX, Nitro, or SEV enclave. Consumer's plugin verifies enclave attestation (measurement + platform identity) *before* sending the encrypted body. Enclave signs a post-hoc attestation that the body was discarded. Enclave is the only party that sees plaintext; host OS does not.

Consumer selects `privacy_tier` in config. Tracker's broker enforces at selection time (tier filter applied first).

### 7.2 Exhaustion-proof protocol

Payload attached to every `broker_request` envelope. Two versions:

**v1 baseline — two-signal bundle from the Claude Code bridge.**

```
{
  stop_failure: {                       // from StopFailure{matcher: rate_limit} hook payload
    matcher:     "rate_limit",
    at:          u64,
    error_shape: <plugin-captured>,
  },
  usage_probe: {                        // from `claude -p "/usage"` run at verification
    at:          u64,
    output:      <parsed /usage result>,
  },
  captured_at:     u64,
  nonce:           bytes16,
}
```

*Amended 2026-04-25: ProofV1 has no standalone signature — the enclosing `EnvelopeSigned.consumer_sig` covers the proof's bytes. Standalone proof verification is not exercised by any v1 use case; if a future use case (e.g., gossiped reputation audit) requires it, reintroduce a proof-internal sig in a versioned amendment. Schema lives in `shared/exhaustionproof/proof.proto`.*

Trust rests on L1 (the reference plugin honors the two-stage gate) plus the correlation of two independent Claude-Code-sourced signals — a forker has to fabricate both plausibly. L3 catches systematic lying over time.

**v2 hardened — Claude-Code-issued signed attestation.**

The earlier MPC-TLS / zkTLS path is blocked under this architecture: the TLS session to Anthropic lives inside Claude Code, not the plugin. v2 is re-specced as a feature request upstream for a Claude-Code-issued signed assertion of the form `{account_fingerprint, at, rate_limited: bool, nonce, cc_sig}`. When such a primitive exists, it replaces the two-signal bundle and gives cryptographic rather than observational assurance. See §9.

### 7.3 Reputation-based abuse detection

Tracker maintains rolling 7-day windows per identity with the following signals:

**Consumer signals:** network-usage rate, claimed-exhaustion rate, ratio of network requests to locally-served requests (requires optional telemetry opt-in).

**Seeder signals:** offer acceptance rate, completion rate, tampering complaints, per-model cost report variance vs. peer seeders.

**Action.** Per-signal z-score computed against the regional population. `|z| > 4` → identity enters **audit mode**: L2 proofs mandatory, telemetry required. Three audit-mode events within 7 days → **frozen**: ledger flag, seeder offers rejected, consumer broker requests rejected, federation-gossiped revocation.

### 7.4 Content-attestation — open problem

If a consumer submits policy-violating content and the seeder's Anthropic account is banned, the seeder's only defense is the audit trail (§4.3), which proves *forwarding* not *content acceptability*. Standard tier cannot fully eliminate this risk. TEE tier closes it for enclave-class seeders by running a content policy check inside the enclave before forwarding. **The plugin install UX must surface this risk prominently for non-TEE seeders.** See §9 for the distributed-classifier direction.

---

## 8. Failure modes

| Failure | Behavior |
|---|---|
| Seeder drops mid-stream | Per Q7c(i), HTTP error propagates to Claude Code CLI. No usage report; no ledger entry; no credit debited. |
| Tracker unreachable | Consumer's plugin falls back to next-nearest known tracker from peer list. Until reconnect, no network fallback — CLI sees 429s from Claude Code. |
| Tracker rewrites its own ledger | Detected via archived Merkle roots at peer trackers. Federation alert, depeering, consumer migration. |
| Seeder reports inflated usage | L3 reputation catches the outlier over time. Consumer may refuse to counter-sign at step 17 — goes to tracker dispute with seeder audit log + tracker timing records. |
| Replayed exhaustion proof | Nonce + 60s freshness in proof. Tracker rejects duplicates. |
| Network partition | Each side serves its own region; cross-region transfers queue. On reunion: any `transfer_uuid` appearing on both sides with conflicting sigs → equivocation handling (§6.5). |
| Full seeder pool exhausted | Broker returns `NO_CAPACITY`. Consumer CLI sees 503-equivalent. User retries later. |
| Plugin version mismatch | Protocol version in handshake. Incompatible versions refused with upgrade message. |
| Identity key compromise | User enrolls a new identity (another Anthropic challenge), announces revocation of old key signed by new one. **Credits on old key are lost** — v1 has no key-rotation scheme. |
| Consumer unreachable at settlement (steps 17–18) | Tracker holds `settlement_request` pending for 15 minutes. If consumer does not counter-sign within the window, tracker finalizes the entry with `consumer_sig_missing=true` flag: seeder receives credit (service was rendered), consumer is debited by default. Consumer may later open a dispute with the tracker providing counter-evidence (e.g., the response was corrupt); approved disputes reverse the debit. Default state: you pay for what the seeder verifiably delivered. |

---

## 9. Future work

- **v2 exhaustion-proof upgrade.** Pursue a Claude-Code-issued signed rate-limit attestation upstream; this replaces the earlier zkTLS plan which is unreachable under the Claude Code bridge architecture.
- **TEE tier redesign.** The original enclave model assumed the seeder passed an API token into the enclave; under the Claude Code bridge architecture, this premise no longer holds. The TEE tier needs a new design (see TEE-tier spec top-matter). Options include repurposing the enclave as a content-policy gateway on the seeder's host.
- **Key rotation** that preserves credit history across key changes.
- **Distributed content policy** (§7.4): gossip-based prompt classifier to reduce per-seeder ban exposure in standard tier.
- **Tracker-less DHT mode** (§3): fully P2P variant replacing regional trackers with Kademlia-style DHT.
- **Bandwidth-as-credit**: credit tracker-relay operators for data-path bandwidth they contribute when hole-punching fails.
- **Cross-implementation compatibility**: protocol conformance test suite so alternative plugin implementations can interoperate.

---

## 10. Interfaces (cross-cutting)

Each interface below is the contract a subsystem exposes. Subsystem specs will detail wire format, error codes, and versioning.

### 10.1 Plugin ↔ Tracker (control plane)

- `enroll(identity_proof, role)` → `enrollment_token`
- `connect(identity, auth_sig)` → persistent stream for heartbeats, availability, offers
- `broker_request(envelope)` → `seeder_assignment | NO_CAPACITY`
- `usage_report(request_id, counts, sig)` → `ledger_entry_preimage`
- `settle(entry_preimage, consumer_sig)` → `finalized_entry`
- `balance(identity_id)` → `signed_snapshot`
- `transfer_request(identity_id, amount, dest_region)` → `transfer_proof`

### 10.2 Tracker ↔ Tracker (federation)

- `peer_handshake(tracker_id, cert)` → `peering_accept | reject`
- `merkle_attestation(hour, root, sig)` — gossip
- `transfer_proof(…)` — counterparty accepts or rejects transfer
- `revocation(identity_id, reason, sig)` — gossip

### 10.3 Seeder ↔ Consumer (data plane)

- NAT hole-punch handshake (tracker-mediated rendezvous; exact protocol TBD in subsystem spec — candidates: ICE/STUN/TURN stack, or a simpler custom UDP rendezvous)
- Encrypted tunnel: TLS 1.3 with consumer+seeder ephemeral keys authenticated against the identities from the tracker offer
- Body frame: `request_body` (full Anthropic `/v1/messages` payload)
- Response frame: streamed SSE chunks proxied verbatim

### 10.4 Ledger entry schema

See §6.1.

---

## 11. Glossary

- **Consumer** — a node making requests into the network because its own rate limit is exhausted.
- **Seeder** — a node accepting and forwarding requests using its own Anthropic token.
- **Tracker** — a server coordinating a region's registry, broker, and ledger.
- **Region** — a latency neighborhood; one tracker per region (or per-redundancy cluster).
- **Tier** — privacy class of a seeder: `standard` or `tee_attested`.
- **Credit** — unit of account in the ledger; 1 credit roughly equals 1000 input tokens on Opus (exact weighting TBD per model).
- **Entry** — one settled request in the ledger (§6.1).
- **Exhaustion proof** — evidence that the consumer's own Anthropic token returned `429` (§7.2).
- **L1 / L2 / L3** — the three layers of the rate-limit exhaustion gate (client-side / cryptographic / reputation).
- **Starter grant** — one-time 50-credit allocation on first verified enrollment.

---

## 12. Subsystem specs

Each subsystem has its own design spec. All inherit the architectural decisions in §0 and must maintain the invariants in §2.2.

| # | Subsystem | Spec | Role |
|---|---|---|---|
| 1 | Consumer / seeder plugin | [plugin/2026-04-22-plugin-design.md](./plugin/2026-04-22-plugin-design.md) | User-installed Claude Code plugin. Local HTTPS proxy, seeder offer handler, identity & tracker client. |
| 2 | Regional tracker | [tracker/2026-04-22-tracker-design.md](./tracker/2026-04-22-tracker-design.md) | Coordination server. Seeder registry, request broker, ledger owner, STUN/TURN rendezvous. |
| 3 | Federation protocol | [federation/2026-04-22-federation-design.md](./federation/2026-04-22-federation-design.md) | Inter-tracker protocol. Merkle-root gossip, cross-region credit transfers, revocation, peer exchange. |
| 4 | Credit ledger | [ledger/2026-04-22-ledger-design.md](./ledger/2026-04-22-ledger-design.md) | Append-only signed log. Entry format, hash chaining, Merkle roots, balance projection, equivocation. |
| 5 | Exhaustion proof | [exhaustion-proof/2026-04-22-exhaustion-proof-design.md](./exhaustion-proof/2026-04-22-exhaustion-proof-design.md) | Cryptographic artifact attesting to a real `429` from Anthropic. v1 self-attested; v2 MPC-TLS notarized. |
| 6 | Reputation | [reputation/2026-04-22-reputation-design.md](./reputation/2026-04-22-reputation-design.md) | L3 backstop. Signal collection, z-score detection, OK/AUDIT/FROZEN state machine, revocation issuance. |
| 7 | TEE-attested tier | [tee-tier/2026-04-22-tee-tier-design.md](./tee-tier/2026-04-22-tee-tier-design.md) | Hardened privacy tier. Enclave attestation, image distribution, runtime contract, optional content policy. |

### 12.1 Dependency graph

```
         ┌──────────────┐
         │    ledger    │◄──── read/write
         └──────▲───────┘
                │
       ┌────────┴────────┐
       │                 │
       ▼                 ▼
  ┌─────────┐       ┌─────────┐
  │ tracker │◄─────►│federation│
  └────▲────┘       └─────────┘
       │
       │ RPC
       │
  ┌────┴────┐
  │ plugin  │
  └────┬────┘
       │
       ▼
  ┌────────────────────┐
  │ exhaustion-proof   │ (plugin generates, tracker validates)
  └────────────────────┘

  reputation ◄── (side-car to tracker; reads ledger entries, writes scores)
  tee-tier   ◄── (augments plugin's seeder role; tracker validates attestations)
```

### 12.2 Reading order

For first-time readers, recommended order:
1. This document (§0–§11).
2. Ledger — the simplest and most foundational data structure.
3. Plugin — the user-facing surface; grounds the rest in concrete UX.
4. Tracker — the server that ties plugin and ledger together.
5. Federation — how regions interoperate.
6. Exhaustion proof — the cryptographic glue for L2.
7. Reputation — the L3 backstop.
8. TEE tier — the optional hardened privacy path.

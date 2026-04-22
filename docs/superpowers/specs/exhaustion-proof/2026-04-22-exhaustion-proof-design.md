# Exhaustion Proof — Subsystem Design Spec

| Field | Value |
|---|---|
| Parent | [Token-Bay Architecture](../2026-04-22-token-bay-architecture-design.md) |
| Status | Design draft (revised for Claude Code bridge architecture) |
| Date | 2026-04-22 |
| Scope | The signed artifact attached to every `broker_request` envelope, attesting that the consumer's Claude Code account just hit a rate limit. Defines the v1 two-signal bundle built from the `StopFailure{rate_limit}` hook payload and a `claude -p "/usage"` probe, the v2 target (a Claude-Code-issued signed rate-limit attestation), tracker-side validation, and migration. |

## 1. Purpose

Close the L2 layer of the rate-limit exhaustion gate (parent spec §7.2). Without a proof, a consumer who has forked the plugin and disabled L1 could flood the network with requests they could serve locally — leeching instead of using overflow.

A valid proof binds together: a specific consumer identity, a specific recent rate-limit condition reported by the consumer's own Claude Code, and a specific request envelope (so the proof can't be replayed across requests).

## 2. Versioning

### 2.1 v1 — two-signal bundle from the Claude Code bridge

Baseline. The consumer plugin does not see the raw TLS session to Anthropic — that session lives inside Claude Code. Instead, the plugin collects two independent signals from Claude Code itself and signs them:

- **Signal A:** the `StopFailure` hook payload with `matcher: rate_limit`, captured when Claude Code's most recent turn ends because of a rate-limit error.
- **Signal B:** the output of `claude -p "/usage"` run with tool-disabling flags immediately after the hook fires, as an independent probe of rate-limit state.

Both signals are bundled under a single consumer signature. A forker must fabricate both coherently — harder than faking one, though still not cryptographic. Trust ultimately rests on L1 (reference plugin) with L3 reputation as the safety net.

### 2.2 v2 — Claude-Code-issued signed attestation (target)

Hardened. v2 asks Claude Code to emit a first-party signed assertion of rate-limit state, e.g. a primitive `claude identity attest-ratelimit <nonce>` that returns `{account_fingerprint, at, rate_limited: bool, nonce, cc_sig}`. With such a primitive, the attestation is cryptographically bound to the user's Claude Code account (via whatever signing material Claude Code uses), and the plugin can no longer forge by fabricating signals.

**v2 depends on upstream cooperation.** If Claude Code never exposes the primitive, v2 is unreachable under this architecture. The prior MPC-TLS / zkTLS notary plan is abandoned because the TLS session to Anthropic is not observable from the plugin.

v1 and v2 coexist during migration: envelopes carry a `version` field; trackers accept v1 until a federation-wide flag day flips the minimum to v2.

## 3. v1 format

### 3.1 Structure

```text
ExhaustionProofV1 {
  version:            u8                 // = 1
  consumer_id:        bytes32
  envelope_hash:      bytes32            // binds proof to the specific envelope

  stop_failure: {
    matcher:          string             // must be "rate_limit"
    at:               u64                // hook-fire timestamp
    error_shape:      bytes              // plugin-captured StopFailure payload (structured JSON if available, else text)
  }

  usage_probe: {
    at:               u64                // when `claude -p "/usage"` was run
    output:           bytes              // raw or parsed /usage output
    output_format:    string             // "json" | "text"
  }

  captured_at:        u64                // plugin's wallclock when the envelope was built
  nonce:              bytes16            // unique per proof (freshness)
  consumer_sig:       bytes64            // Ed25519 over all above
}
```

### 3.2 v1 validation (tracker side)

1. `version == 1`. If greater and tracker supports it, dispatch to v2 validator.
2. `consumer_id` matches the envelope's consumer_sig identity.
3. `envelope_hash` matches the envelope.
4. `stop_failure.matcher == "rate_limit"`.
5. `now - stop_failure.at <= freshness_window` (default 60s).
6. `now - usage_probe.at <= freshness_window`.
7. `|usage_probe.at - stop_failure.at| <= 30` seconds (the two signals must correlate — a user who "hit a limit 10 minutes ago" probing /usage now is uncorrelated).
8. `usage_probe.output` parses successfully and reports a rate-limit-exhaustion state (exact parse rules track Claude Code's `/usage` output format; see §6).
9. `now - captured_at <= 120` seconds.
10. `nonce` not in the replay cache (TTL 240s).
11. `consumer_sig` verifies under `consumer_id`'s pubkey.

On all-pass: tracker accepts, records nonce in replay cache.

### 3.3 Forgery surface in v1

- **Plugin fork (primary threat).** User runs a modified plugin that fabricates both signals. Two-signal correlation raises the bar versus single-signal v1 (the original design), but does not eliminate the threat. Detection: L3 statistical outliers (reputation spec §6.1).
- **Replay within window.** Mitigated by nonce + freshness.
- **Cross-request replay (different envelope).** Mitigated by `envelope_hash`.
- **Signal inconsistency.** If a forker fakes only one signal (e.g., a plausible `StopFailure` but a `/usage` output showing headroom), step 7/8 catches it. Reputation spec tracks `proof_fidelity_level`.
- **Shared-identity abuse.** Key compromise → revocation flow handles it.

v1 accepts this forgery surface as the explicit tradeoff for a baseline deployable without Claude Code upstream cooperation.

## 4. v2 format

### 4.1 Structure (target, pending upstream primitive)

```text
ExhaustionProofV2 {
  version:            u8                 // = 2
  consumer_id:        bytes32
  envelope_hash:      bytes32

  cc_attestation: {
    account_fingerprint: bytes32        // opaque account ID assigned by Claude Code
    rate_limited:     bool              // must be true for proof to be valid
    at:               u64
    nonce:            bytes16           // consumer-supplied at attestation time
    cc_pubkey_id:     bytes32          // which Claude Code attestation key signed
    cc_sig:           bytes64           // Claude Code's signature
  }

  captured_at:        u64
  nonce:              bytes16            // envelope-level nonce (distinct from cc_attestation.nonce)
  consumer_sig:       bytes64            // Ed25519 over all above
}
```

### 4.2 v2 validation

1. Checks 1–3 from §3.2 (version, consumer_id, envelope_hash).
2. `cc_attestation.rate_limited == true`.
3. `now - cc_attestation.at <= freshness_window`.
4. `cc_pubkey_id` in the tracker's accepted-Claude-Code-attestation-keys list.
5. `cc_sig` verifies under `cc_pubkey_id`.
6. `cc_attestation.nonce` derived in a way the consumer cannot freely choose post-hoc (implementation-dependent — ideally bound to `envelope_hash` or similar).
7. `consumer_sig` verifies.
8. `nonce` not in replay cache.

### 4.3 What v2 prevents

- **Forged rate-limit claim.** Consumer cannot produce `cc_sig` without participating in a live Claude Code attestation flow.
- **Cross-identity replay.** `account_fingerprint` binds to one account; `cc_sig` over that plus `envelope_hash` prevents re-use for different envelopes.
- **Stale attestation replay.** Freshness window + nonce.

v2 does not prevent:
- **Claude Code bugs / compromise.** If Claude Code itself signs a false attestation, v2 is defeated. Out of scope; this is the same threat surface as "trust Claude Code to enforce tool restrictions on the seeder side."
- **A consumer using their own account legitimately then also using the network** — not a forgery; this is the intended pattern.

## 5. Upstream dependency on Claude Code

v2 is the primary place this architecture interfaces with Claude Code as a first-party capability. The feature request is:

> **Claude Code should expose a command (CLI subcommand, plugin API, or similar) that emits a signed attestation of current rate-limit state, bound to a caller-provided nonce and the authenticated account's identity.**

Possible shapes:

- `claude identity attest-ratelimit --nonce <hex>` → returns signed JSON
- A plugin-callable function in the Claude Code plugin API
- An entry in a generalized `claude identity sign <claim>` family

### 5.1 Stop-gap if v2 is blocked indefinitely

If Claude Code never adds the primitive, v1 remains the only option and the network lives with the honor-system tradeoff. Reputation-based abuse detection (L3) is the mitigating layer. Operators of sensitive deployments may choose to run the network in closed-community mode (federation §6 allows this) where admission control substitutes for cryptographic exhaustion proofs.

## 6. Validator configuration

Tracker configuration:

```yaml
exhaustion_proof:
  min_version: 1                   # accept v1 or v2 (v1 until flag day; then 2)

  v1:
    freshness_window_s: 60
    signal_correlation_window_s: 30
    usage_parse_rules:
      # Regex / JSON-path patterns describing what "exhaustion" looks like
      # in `claude -p "/usage"` output. Updated when Claude Code changes
      # the /usage output format.
      - format: "json"
        exhausted_when: "$.status == 'limited' || $.remaining == 0"
      - format: "text"
        exhausted_when_regex: "^.*(rate[- ]limit|exhausted|exceeded).*$"

  v2:
    accepted_cc_attestation_keys:  # Claude Code's public attestation keys
      - cc_pubkey_id: <base64>
        rotated_at: 2026-04-22

  nonce_cache_ttl_s: 240
  envelope_freshness_window_s: 120
```

## 7. Migration plan

1. **v1 deploy.** Release plugin and tracker with v1 validation. Plugins emit v1 two-signal proofs using the `StopFailure{rate_limit}` hook and `claude -p "/usage"` probe.
2. **Monitor signal fidelity.** Reputation subsystem tracks `proof_fidelity_level` (full / partial / degraded). Populations where many users are on older Claude Code versions may show more "degraded" proofs — inform Claude Code release cadence.
3. **v2 opt-in phase** (gated on Claude Code adding the attestation primitive). Plugins and trackers ship v2 capability; tracker accepts v1 too. Collect data on v2 availability per Claude Code version.
4. **v2 soft-required.** Trackers log warnings on v1 acceptance; plugins default to v2 when the primitive is available locally.
5. **v2 flag day.** Coordinated federation-wide `min_version: 2` flip. Plugins without access to the v2 primitive degrade to read-only (can seed, cannot consume).

Timeline is dependent on upstream. Realistic: v1 lives in production for an extended window.

## 8. Failure handling

| Failure | Behavior |
|---|---|
| `claude -p "/usage"` invocation fails (binary missing, Claude Code crashes) | Plugin cannot build v1 proof; surfaces error to user. Fallback unavailable until Claude Code is reachable. |
| `/usage` output format unrecognized | Plugin produces a "degraded" proof; tracker may accept with L3 reputation penalty or reject per config. Update `usage_parse_rules` to handle new format. |
| Signal correlation fails (§3.2 step 7/8) | Tracker rejects with `EXHAUSTION_PROOF_INCONSISTENT`. Consumer retries after a fresh `StopFailure` event. |
| Clock skew between plugin and tracker | Freshness window is 60s; skew > 30s starts failing. Both parties should run NTP. |
| Replay attack | Nonce cache (TTL 240s) catches. Replay storms are L3 abuse. |
| Claude Code version change breaks `/usage` parsing | Plugin startup runs a conformance test invoking `claude -p "/usage"` with a known account and verifying parse. On failure, disable consumer role until rules updated. |
| v2 attestation key rotated by Claude Code | Tracker updates `accepted_cc_attestation_keys`; existing v2 proofs under old key honored until expiry. |

## 9. Open questions

- **Exact shape of the `StopFailure{rate_limit}` payload.** Determines what structured fields are available for `stop_failure.error_shape`. Plugin spec §10 flags this same question.
- **Exact output format of `claude -p "/usage"`.** Is it stable JSON, text, or both? `usage_parse_rules` anchor the schema; need to track Claude Code release notes for format changes.
- **Will Claude Code ever add a rate-limit attestation primitive?** Upstream feature request. Without it, v2 is unreachable and v1 is the permanent answer.
- **Can the plugin's `/usage` probe itself hit a rate limit?** The probe is a real API call. Pathological case: consumer is so rate-limited that `/usage` also fails. In that case the plugin has only the `StopFailure` signal — degraded proof, accepted with L3 penalty.
- **Nonce binding in v2.** Ideally `cc_attestation.nonce` is derived from `envelope_hash` so a single attestation cannot cover multiple envelopes. Requires Claude Code to accept nonce input at attestation time.

## 10. Acceptance criteria

- Tracker rejects envelopes without a valid proof (v1 or v2 per config).
- Nonce replay within 240s is rejected.
- `envelope_hash` tamper causes rejection.
- A proof where `stop_failure.at` and `usage_probe.at` differ by > 30s is rejected.
- A proof where `/usage` output indicates headroom but `StopFailure` claims exhaustion is rejected.
- v1→v2 migration: a tracker configured `min_version: 2` rejects v1 proofs from older plugins with a clear error code.
- Plugin startup conformance test verifies `/usage` parse rules against the installed `claude` binary before advertising consumer capability.

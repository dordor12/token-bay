# Consumer / Seeder Plugin — Subsystem Design Spec

| Field | Value |
|---|---|
| Parent | [Token-Bay Architecture](../2026-04-22-token-bay-architecture-design.md) |
| Status | Design draft |
| Date | 2026-04-22 |
| Scope | The Claude Code plugin running on each participant's host. Same binary serves both the consumer and seeder roles. Defines identity/enrollment, tracker client, consumer-side fallback flow, seeder-side offer handler, idle/availability logic, audit log, and config. |

## 1. Purpose

Provide a user-installable Claude Code plugin that:

- Detects when the user's own Claude Code session hits its rate limit, and offers a transparent fallback through the Token-Bay network.
- Offers the user's own Claude Code capacity as a seeder when their idle policy is active.
- Maintains the user's identity keypair, tracker connection, and audit log.

**Critical architectural property (this rewrite):** the plugin does not store, read, or proxy any Anthropic API key. All Anthropic-bound traffic goes through the **Claude Code CLI bridge** — a thin wrapper around whatever `claude` subcommand provides authenticated API access. The plugin is purely a coordination layer; Claude Code remains the sole credential holder.

## 2. Interfaces

### 2.1 Exposed (locally)

- **Slash commands** (registered with Claude Code):
  - `/token-bay enroll` — first-run setup.
  - `/token-bay status` — show tracker connection, balance, seeder availability.
  - `/token-bay balance` — signed balance snapshot.
  - `/token-bay fallback` — manually route the current message via the network (invoked when Claude Code has hit a limit).
  - `/token-bay seed on|off` — immediate availability toggle (overrides idle policy for the current window).
  - `/token-bay logs` — audit log viewer.
- **Hooks** (Claude Code extension points):
  - **`StopFailure` with `matcher: rate_limit`** — the primary trigger. Fires when a turn ends because the Anthropic API returned a rate-limit error. The hook payload gives the plugin structured access to the failure (status, headers, timestamp). Other `StopFailure` matchers (`authentication_failed`, `billing_error`, `invalid_request`, `server_error`, `max_output_tokens`, `unknown`) are **not** handled by the plugin — none of them are cases where network fallback is appropriate. Only `rate_limit` triggers the fallback flow.
  - `UserPromptSubmit` — optional pre-flight: if the plugin's cached state says the user is currently rate-limited (confirmed within the last N minutes), the plugin can offer fallback before even attempting the local call, saving a round-trip to Anthropic.
  - `SessionStart` / `SessionEnd` — update availability state based on whether the user is actively using Claude Code (feeds the seeder idle policy, §6.3).
- **Background service** (sidecar process started at Claude Code launch): maintains the long-lived tracker connection, handles incoming seeder offers, runs the availability state machine. The sidecar has no UI of its own; it surfaces state through slash commands and hooks.

### 2.2 Consumed

- **Claude Code CLI bridge** — the mechanism by which this plugin invokes authenticated Anthropic API calls. Detailed in §5 and §6; flagged as the primary open design question in §10.
- Tracker client API (from `tracker` subsystem): `enroll`, `connect`, `broker_request`, `usage_report`, `settle`, `balance`, `transfer_request`.
- Peer tunnel protocol (from parent §10.3; QUIC-based).
- Local filesystem for audit log and config.

## 3. Module layout

```
plugin/
  bin/                       -- slash-command entry points
  sidecar/                   -- long-running background service
  hooks/                     -- Claude Code hook handlers
  tracker-client/            -- long-lived connection + RPCs
  identity/                  -- Ed25519 keypair mgmt, enrollment flow
  cc-bridge/                 -- Claude Code CLI bridge (see §5 and §6)
  rate-limit-detector/       -- parses Claude Code errors / output for 429 signals
  exhaustion-proof/          -- v1 self-attest (stub for v2 zkTLS)
  tunnel/                    -- consumer ↔ seeder P2P tunnel
  audit-log/                 -- append-only local log
  config/                    -- YAML parsing, validation, hot-reload
```

(Layout is illustrative.)

## 4. Enrollment & identity

### 4.1 First run

```
$ /token-bay enroll
→ Generating Ed25519 identity keypair…
→ Saved to ~/.token-bay/identity.key
→ Verifying Claude Code account binding via the Claude Code CLI bridge…
→ Identity bound to Claude Code account: ****@example.com (via OAuth session).
→ Requesting bootstrap tracker list…
→ Connected to tracker: eu-central-1.bootstrap.token-bay.dev
→ Starter grant applied: 50 credits
Ready.
```

### 4.2 Identity-challenge protocol (revised)

The parent spec's Q5(b) decision was "proof-of-Anthropic-account." Since the plugin no longer has direct access to an API token, the proof has to go through Claude Code. Three candidate mechanisms, in decreasing order of preference:

**Option α — Claude Code emits a signed identity assertion.** If Claude Code exposes (now or later) a command like `claude identity sign <nonce>` that returns a signature over `nonce || account_fingerprint` using the account's auth material, the plugin uses that directly. Cleanest; depends on Claude Code adding the primitive.

**Option β — Bridge-issued probe.** Plugin invokes the Claude Code bridge with a minimal prompt containing a tracker-issued nonce, e.g. "Respond with exactly `ACK:<nonce>`." The response comes back through the authenticated channel; tracker observes that the response arrived from an authenticated Claude Code session. Weaker than α (observational, not cryptographic), but works today without Claude Code changes. Account fingerprint is derived from Claude Code metadata surfaced via `claude config` or equivalent (what exactly gets exposed is open — see §10).

**Option γ — Drop Anthropic-account binding; fall back to pure self-sovereign identity.** Identity is just a local Ed25519 keypair with no Anthropic tie. Sybil resistance downgrades from Q5(b) to something closer to Q5(a)/(c) (proof-of-work or web-of-trust). Parent spec would need a corresponding amendment.

**v1 direction:** start with β, plan for α when Claude Code ships a primitive. If neither path works for a given deployment (e.g., operator cannot rely on any identity leak from the bridge), fall back to γ and accept the weaker sybil posture — this is a per-tracker config.

### 4.3 Key storage

- `~/.token-bay/identity.key` — Ed25519 private key, permissions `0600`.
- OS keychain integration optional.
- **No Anthropic API key is ever written by this plugin.** Anthropic authentication is entirely owned by Claude Code.

## 5. Consumer role — fallback pipeline

### 5.1 Trigger (two-stage)

The plugin does **not** intercept Claude Code's HTTPS traffic. Instead, it reacts to Claude Code's own `StopFailure` hook and then **independently re-verifies** the rate limit before spending network credits. Two activation paths:

- **Automatic.** A turn ends with `StopFailure{matcher: rate_limit, ...}`. The plugin runs the verification in §5.2, and on confirmed exhaustion, injects a one-turn confirmation to the user: "You've hit your Claude rate limit. Retry via Token-Bay network? (≈ N credits; your balance is M)."
- **Manual.** User runs `/token-bay fallback`. The plugin still runs the verification in §5.2; if `/usage` reports plenty of headroom, the plugin refuses with an informational message (the user's rate limit isn't actually exhausted; no reason to spend credits).

### 5.2 Rate-limit verification via `claude /usage`

Before routing anything to the network, the plugin invokes Claude Code's own `/usage` slash command through the CLI bridge to confirm current rate-limit state. This is a second, independent signal from Claude Code saying "yes, this account is at its limit right now" — not just reacting to a single failed turn which might be transient or race-conditioned.

```bash
claude -p "/usage" \
  --output-format json \
  --disallowedTools "*" \
  --mcp-config /dev/null
```

Plugin parses the output and checks for rate-limit-exhaustion signals (e.g., usage ≥ quota for the relevant window, or the response explicitly reporting "rate limited"). Exact parsing rules follow Claude Code's `/usage` output format.

**Verdict matrix:**

| `StopFailure` fired | `/usage` confirms exhaustion | Action |
|---|---|---|
| yes | yes | **Proceed with fallback** (this is the happy path). |
| yes | no | Ambiguous — `StopFailure` may have been a transient error or a stale race. Plugin offers retry-without-fallback first; falls back only if the user insists (manual `/token-bay fallback`). |
| no (manual invocation) | yes | Proceed with fallback. |
| no | no | Refuse fallback. Inform the user their local rate limit has headroom. |

The two-stage check materially improves the exhaustion-proof fidelity for v1 (see §5.4): the proof bundle carries both the `StopFailure` event and the `/usage` output, each independently signed by the consumer, giving the tracker + reputation system two correlated signals instead of one.

### 5.3 Fallback flow

1. Capture the current user prompt + conversation context from the Claude Code session (via the plugin's access to conversation state).
2. The exhaustion proof material is already in hand: the `StopFailure` event payload from §5.1 plus the `/usage` output from §5.2. See §5.4 for how it folds into the proof.
3. Build envelope (parent spec §2 step 3):
   ```
   {
     model, max_input_tokens, max_output_tokens,
     tier: config.privacy_tier,
     body_hash,
     exhaustion_proof: { response, timestamp, nonce, consumer_sig },
     consumer_sig,
     balance_proof: signed_snapshot
   }
   ```
4. `tracker.broker_request(envelope)`. On `NO_CAPACITY`, surface error to the user via a Claude Code message.
5. On assignment, open tunnel to the assigned seeder. On tunnel failure, surface error.
6. Send encrypted conversation body over the tunnel.
7. Receive streamed response chunks from the seeder and **inject them back into the Claude Code conversation** as if they had come from Anthropic. The exact injection mechanism is the plugin-to-Claude-Code boundary described in §10.
8. On stream end: counter-sign the tracker's settlement request.
9. On mid-stream tunnel error: surface error to the user per Q7c(i); conversation state reflects the partial response (user can retry).

### 5.4 Exhaustion-proof capture

The plugin doesn't see the raw TLS session to Anthropic, so v1 proofs are built from Claude Code's own exposed signals. Because of the two-stage verification in §5.2, the proof bundle carries **two** independent fields:

```
ExhaustionProofV1 {
  version:        1,
  consumer_id,
  envelope_hash,
  stop_failure: {                    // from the StopFailure hook payload
    matcher:     "rate_limit",
    at:          <timestamp>,
    error_shape: <whatever Claude Code surfaces>,
  },
  usage_probe: {                     // from `claude -p "/usage"` run at verification
    at:          <timestamp>,
    output:      <parsed /usage result>,
  },
  captured_at,
  nonce,
  consumer_sig,
}
```

Both fields are under the consumer's signature. The tracker's v1 validator (exhaustion-proof spec §3.2) checks freshness (both `at` values within 60s) and structure; v1 remains honor-system at the cryptographic layer, but the correlation of two independent Claude-Code-reported signals makes forgery noticeably harder — a forker must fake both the `StopFailure` payload and a coherent `/usage` output.

The v2 zkTLS path (MPC-TLS notarized) remains blocked without Claude Code cooperation to expose the raw transcript. In this architecture, v2 may instead be re-specced as **"a Claude-Code-issued signed attestation of rate-limit state"** — a single Claude Code primitive that cryptographically binds the user's account identity, a fresh nonce, and "is rate-limited: true/false." Calls for a feature request upstream. See exhaustion-proof spec §9 for the re-plan.

## 6. Seeder role

### 6.1 Offer handling

1. Seeder sidecar is connected to tracker with `available=true` (per §6.3).
2. Tracker sends `offer{consumer_id, envelope_hash, terms}`.
3. Seeder evaluates capability: supports requested `model`, supports requested `tier`, has headroom (no recent rate-limit errors from local Claude Code sessions).
4. On accept: receive consumer's encrypted request body via tunnel.
5. **Invoke the Claude Code CLI bridge** with the consumer's prompt as input. See §6.2 for the bridge contract.
6. Stream the bridge's output back over the tunnel.
7. On stream end: send `usage_report(request_id, input_tokens, output_tokens, model, seeder_sig)` to tracker.

### 6.2 The Claude Code CLI bridge

The bridge is a single `claude -p "<query>"` invocation per request, executed with flags that disable every side-effecting primitive: tool use, MCP servers, and hooks. Safety is achieved by preventing Claude from doing anything except generating text output — no `Bash`, no `Read`, no `Write`, no `WebFetch`, no user MCP tools, no hooks firing arbitrary shell. Bare text in, bare text out.

**Bridge invocation (template):**

```bash
claude -p "<consumer prompt>" \
  --model <requested-model> \
  --output-format stream-json \           # streaming response chunks
  --disallowedTools "*" \                 # disallow ALL tools
  --mcp-config /dev/null \                # neutralize MCP config for this call
  --settings '{"hooks":{}}'               # no hooks this invocation
```

(Exact flag names track the current Claude Code release; the conformance test in §12 validates that the effective configuration disables every side-effecting primitive in that version.)

Run in a **fresh empty temp directory** as CWD so Claude has nothing to see even if a disallowed tool somehow leaks through. Pass the consumer prompt via stdin or an argv quoted string; never via a file Claude might read.

**Safety argument**

With all tools disabled, a malicious consumer prompt can only steer Claude's **text output**. There is no mechanism left for the prompt to read the seeder's filesystem, spawn processes, make non-Anthropic network calls, or touch the seeder's Claude Code state beyond this one-shot API call. The risk surface collapses to "Claude Code's flag enforcement is correct" — a focused, testable property.

**Defense in depth (optional)**

Seeders who want belt-and-braces can still run the bridge inside `bubblewrap` / `firejail` / a container, restricting filesystem writes and network to `api.anthropic.com` only. For v1 this is **optional and recommended for high-value seeders** — the primary guarantee is the flag set. Sandbox configuration is reflected in the config (§9) but defaults to disabled.

**What the bridge returns**

- Streaming chunks in stream-json format (Anthropic SSE content forwarded verbatim through Claude Code's streaming output).
- Final `usage` block with `input_tokens` and `output_tokens` — used for `usage_report` metering.
- Exit status 0 on success, non-zero on failure (rate limit, network error, auth failure).

**Open bridge questions**

- **Exact airtight flag combination per Claude Code release.** The conformance test in §12 runs a suite of adversarial prompts (attempts at `Bash` execution, file reads, URL fetches, MCP calls) and asserts none produces any observable side effect. Any new Claude Code release requires re-running the suite.
- **Streaming support in `-p` mode.** If the current release doesn't stream in `-p` mode, v1 degrades to request-response only for seeders; streaming consumers degrade gracefully (treat the full response as a single chunk). Capability is advertised to the tracker so the broker can match streaming-required consumers with streaming-capable seeders.
- **Token-count exposure.** The bridge output must include Anthropic's usage numbers. If `--output-format stream-json` doesn't include them, fall back to estimating input tokens via tokenizer and output tokens via response length (less accurate; seeders report slightly noisier numbers, which is fine since §7.3 reputation catches systematic lying).

### 6.3 Availability logic

Seeder advertises `available=true` iff **all**:

- **Idle policy** (from config):
  - `mode: scheduled` → current local time ∈ `window` (e.g. `02:00-06:00`).
  - `mode: always_on` → always true.
- **Grace since last activity**: no active Claude Code session observed in the last `activity_grace` minutes (SessionStart/SessionEnd hooks tracked by the sidecar).
- **Headroom**: the local Claude Code has not reported a rate-limit error in the last `headroom_window` minutes (default 15m). Conservative heuristic since the plugin doesn't see usage directly.

On any condition flipping to false, plugin immediately sends `available=false`. In-flight offers complete; no new offers accepted.

### 6.4 In-flight abort policy

If user starts a Claude Code session while seeder has an in-flight request:
- Seeder completes the in-flight bridge call (cancelling mid-stream would leave the consumer's request in a bad state).
- No new offers accepted during or after. Seeder immediately sends `available=false`.
- If the bridge call contention causes the user's own session to hit a rate limit: plugin falls back to the network (§5) — which is the intended behavior.

## 7. Peer tunnel

### 7.1 Setup

Unchanged from previous revision: tracker returns `{seeder_addr, seeder_ephemeral_pubkey}` to consumer; both sides attempt UDP hole-punch via tracker STUN, fall back to tracker TURN relay. QUIC-based transport with identity-pinned TLS handshake.

### 7.2 Wire protocol

- Stream 1: `request_body` → consumer to seeder, one-shot. Contains the serialized conversation context to pass into Claude Code.
- Stream 2: `response_chunks` → seeder to consumer, streamed as the bridge emits tokens. Frame format is the bridge's native streaming format (Anthropic SSE chunks forwarded verbatim where possible).

### 7.3 Tunnel failure semantics

Same as before: hole-punch fail → retry once with TURN; mid-stream disconnect → error to user, no auto-reroute.

## 8. Audit log

Same shape as before. Per-request local log entry (seeder side):

```
{request_id, model, input_tokens, output_tokens, consumer_id_hash,
 started_at, completed_at, tracker_entry_hash?}
```

No content. Append-only. Min 90-day retention. Accessible via `/token-bay logs`.

Consumer also logs:
```
{request_id, served_locally: bool, seeder_id?, cost_credits, timestamp}
```

## 9. Configuration (`~/.token-bay/config.yaml`)

```yaml
role: both                    # consumer | seeder | both
tracker: auto                 # auto (bootstrap) or URL
# NOTE: no anthropic_token_ref — the plugin uses the Claude Code CLI bridge
cc_bridge:
  claude_bin: claude          # path to the claude CLI
  extra_flags: []             # additional flags appended to every invocation
  sandbox:
    enabled: false            # defense-in-depth; disabled by default — flag-based tool-disabling is the primary guarantee
    driver: bubblewrap        # bubblewrap | firejail | docker (Linux only)
idle_policy:
  mode: scheduled             # scheduled | always_on
  window: "02:00-06:00"
  activity_grace: 10m
privacy_tier: standard        # standard | tee_required | tee_preferred
max_spend_per_hour: 500
audit_log_path: ~/.token-bay/audit.log
```

Validated on load; hot-reload on SIGHUP for non-structural fields.

## 10. Open questions

- **Are the tool-disabling flags airtight across every Claude Code release?** The bridge's entire safety argument rests on `--disallowedTools "*" + --mcp-config /dev/null + empty hooks` being honored. A conformance test (§12) must re-validate on every Claude Code upgrade. Any regression is a CVE-class issue and must gate the seeder role.
- **What does Claude Code expose for identity verification?** See §4.2. Ideally a signed-nonce primitive; pragmatically, plugin reads account metadata via `claude config` or equivalent. Until resolved, `option β` is best-effort.
- **What is the exact shape of the `StopFailure{matcher:rate_limit}` payload?** Determines the structured fields available for §5.4. Token-Bay's spec assumes at minimum a timestamp and an error description; richer payloads (retry-after, reset window) feed better proof fidelity and smarter UX.
- **Exact output format of `claude -p "/usage"`.** Is it stable JSON, human-readable text, or both? `--output-format json` is assumed in §5.2; if unavailable, plugin parses text with a tolerance margin.
- **Can a plugin inject streaming response content into the user's Claude Code conversation** in a way that looks native (consumer-side consumption, §5.2 step 7)? If not, plugin has to surface the network response via a separate display mechanism, which is a UX downgrade.
- **Streaming support end-to-end.** If the bridge doesn't stream, the whole network degrades to request-response only. Affects seeder capabilities advertised to the tracker.
- **Multi-session users.** Two Claude Code windows open, both hit a limit. Plugin sidecar serializes fallback requests; need a queueing policy.
- **Windows story.** `bubblewrap` and `firejail` are Linux-only. Windows seeder sandboxing would need WSL2 + bubblewrap, or a Windows-native sandbox driver. v1 may Linux-and-macOS only.

## 11. Failure modes (plugin-specific)

| Failure | Behavior |
|---|---|
| Claude Code not installed / `claude` binary not on PATH | Plugin refuses to start; enrollment fails with a clear error. |
| Claude Code authentication not set up | Enrollment fails at the identity-verification step; user is directed to log into Claude Code first. |
| Claude Code returns a rate-limit error | Expected signal; plugin handles via §5 consumer flow. |
| Claude Code crashes during bridge call (seeder) | Seeder surface error to tracker; request fails per Q7c(i); no usage_report sent. |
| Sandbox driver unavailable (when opted in) | Plugin refuses to start seeder role; consumer role still works. When sandbox is disabled (default), this failure doesn't apply. |
| Claude Code version mismatch — tool-disabling flags renamed | Bridge start-up runs the conformance test (§12) every time the `claude` binary version changes. On failure, seeder role is disabled until the plugin is updated. |
| Tracker down at request time | 503 surfaced to user; no fallback available. |
| Tracker down during seeder advertise | Sidecar retries with backoff. |
| Identity key deleted | Plugin refuses to start; re-enroll required. |
| Audit log disk full | Seeder refuses new offers; consumer still functions. |

## 12. Acceptance criteria

- Plugin is installable via `/plugin` in Claude Code. Enrollment completes without the plugin ever handling an Anthropic API key.
- When the user's Claude Code hits a rate limit, `/token-bay fallback` (and the automatic hook path) successfully routes the same prompt via the network and returns a usable response.
- Seeder mode executes forwarded requests through `claude -p` with the flag set in §6.2; a **bridge conformance test** drives a suite of adversarial prompts (shell commands, file reads, URL fetches, MCP invocations, hook-triggering content) against the configured `claude` binary and asserts zero side effects on the seeder's filesystem, processes, or network. The test runs at plugin startup, on every Claude Code version change, and in CI.
- Switching idle policy takes effect within one heartbeat.
- Audit log is queryable via `/token-bay logs`.
- `/token-bay balance` returns a fresh signed snapshot.
- Plugin recovers from tracker disconnection and resumes seeder-mode within one retry interval.
- **Negative test:** intentional attempt to read `~/.token-bay/` or `~/.claude/credentials*` via a maliciously-crafted forwarded prompt is blocked by the tool-disabling flags and produces no side effect. Sandbox hardening is optional belt-and-braces, not required to pass this test.

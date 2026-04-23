# Consumer / Seeder Plugin — Subsystem Design Spec

| Field | Value |
|---|---|
| Parent | [Token-Bay Architecture](../2026-04-22-token-bay-architecture-design.md) |
| Status | Design draft — revised 2026-04-23 (mid-session redirect) |
| Date | 2026-04-22 · revised 2026-04-23 |
| Scope | The Claude Code plugin running on each participant's host. Same binary serves both the consumer and seeder roles. Defines identity/enrollment, tracker client, consumer-side fallback flow via mid-session redirect, seeder-side offer handler, idle/availability logic, audit log, and config. |

## Revision history

- **2026-04-23** — Mid-session redirect via `~/.claude/settings.json` + Claude Code's settings file watcher. A source-code investigation (see §2.5) confirmed the consumer fallback can redirect an active session's `/v1/messages` calls to the sidecar without restart. Rewrites §5 consumer flow; adds `internal/ccproxy` and `internal/settingsjson` modules; resolves the former §10 open question about response-injection (there is no injection — the sidecar serves an Anthropic-compatible endpoint).
- **2026-04-22** — Initial draft. Claude-Code-bridge architecture replacing the earlier always-on-HTTPS-proxy design. Introduces `StopFailure{rate_limit}` hook + `/usage` two-signal gate; seeder-role `claude -p` with tool-disabling flags.

## 1. Purpose

Provide a user-installable Claude Code plugin that:

- Detects when the user's own Claude Code session hits its rate limit, and offers a transparent **mid-session** fallback through the Token-Bay network.
- Offers the user's own Claude Code capacity as a seeder when their idle policy is active.
- Maintains the user's identity keypair, tracker connection, and audit log.

**Critical architectural properties:**

1. **The plugin does not store, read, or proxy an Anthropic API key.** Consumer-side Anthropic traffic continues to originate from Claude Code itself using its own auth. On the seeder side, prompts are served via `claude -p` (§6.2), which authenticates the seeder's Claude Code against Anthropic directly.

2. **The plugin redirects consumer-side traffic mid-session via settings.json mutation** (§2.5). When the user opts into network fallback, the plugin atomically updates `~/.claude/settings.json` to set `ANTHROPIC_BASE_URL=http://127.0.0.1:PORT`. Claude Code's settings file watcher picks up the change; its next `/v1/messages` call hits the sidecar's `ccproxy` Anthropic-compatible endpoint, which routes through the Token-Bay network. No Claude Code restart, no context loss.

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

- **Claude Code CLI bridge** — `claude -p "/usage"` on the consumer side (§5.2); `claude -p "<prompt>"` with tool-disabling flags on the seeder side (§6.2).
- **Claude Code settings file watcher** — an undocumented internal mechanism (§2.5) that the plugin relies on to make `ANTHROPIC_BASE_URL` changes propagate mid-session. A runtime compatibility probe validates the dependency at plugin startup.
- Tracker client API (from `tracker` subsystem): `enroll`, `connect`, `broker_request`, `usage_report`, `settle`, `balance`, `transfer_request`.
- Peer tunnel protocol (from parent §10.3; QUIC-based).
- Local filesystem for `~/.claude/settings.json`, audit log, identity key, and config.

## 2.5 Mid-session redirect mechanism

### 2.5.1 Discovery

Claude Code's public docs describe `ANTHROPIC_BASE_URL` as read "at startup." That is literally true but practically misleading. A source-code investigation of the upstream `anthropics/claude-code` repo (April 2026) found a chain that effectively re-reads the env var on every API call:

- `~/.claude/settings.json` and related files are watched via **chokidar** (native FS events, not polling). Changes are detected within ~50ms (`src/utils/settings/changeDetector.ts:110-145`).
- On detected change, Claude Code updates `AppState.settings`. A state-change handler fires when `newState.settings.env !== oldState.settings.env` (`src/state/onChangeAppState.ts:156-166`):
  ```js
  if (newState.settings.env !== oldState.settings.env) {
    applyConfigEnvironmentVariables()
  }
  ```
- `applyConfigEnvironmentVariables()` does `Object.assign(process.env, ...)` — including `ANTHROPIC_BASE_URL` — and clears CA / mTLS / proxy caches (`src/utils/managedEnv.ts:187-199`).
- The Anthropic HTTP client is constructed **per-request**, not cached at startup (`src/services/api/client.ts:88` plus the comment at `src/utils/http.ts:30`: *"getAnthropicClient calls this per-request inside withRetry"*). Every call does `new Anthropic({...})` fresh.
- The `@anthropic-ai/sdk` package reads `process.env.ANTHROPIC_BASE_URL` at constructor time.

Combine these: a mutation to `~/.claude/settings.json`'s `env.ANTHROPIC_BASE_URL` → file watcher fires → `process.env` mutated → next outgoing `/v1/messages` call uses the new base URL. Same process, same session, no restart.

The settings source matters: `ANTHROPIC_BASE_URL` is allowed only from `userSettings`, `flagSettings`, or `policySettings` — i.e., `~/.claude/settings.json` or higher. Project-local settings cannot redirect it (intentional security boundary, `src/utils/managedEnv.ts:93-109`). The plugin writes to `~/.claude/settings.json`.

### 2.5.2 The two-phase flow

**Phase A — Enter network mode** (after user consent at §5.3):
1. Plugin hook fires on `StopFailure{rate_limit}`.
2. Plugin confirms via `claude -p "/usage"` (§5.2).
3. Plugin asks user for consent.
4. On consent: sidecar atomically rewrites `~/.claude/settings.json`, setting `env.ANTHROPIC_BASE_URL = "http://127.0.0.1:PORT"` and preserving all other settings verbatim.
5. Claude Code's file watcher detects → AppState updates → `applyConfigEnvironmentVariables()` runs → `process.env.ANTHROPIC_BASE_URL` is mutated.
6. Sidecar flips its own per-session "network mode" flag and records the original env value in a rollback journal.

**Phase B — Request flows through sidecar naturally:**
7. User types the next prompt. Claude Code builds a fresh `Anthropic` client whose constructor reads the new base URL.
8. POST `/v1/messages` goes to `http://127.0.0.1:PORT` — the sidecar's `ccproxy` endpoint.
9. Sidecar looks up session ↔ network-mode state; on hit, routes through the Token-Bay network (not Anthropic). Assembles the exhaustion proof from the cached StopFailure+/usage data.
10. Response streams back via Anthropic-compatible SSE. Claude Code renders it as a normal turn — indistinguishable from direct Anthropic output.

**Exit** (§5.5): time-based / explicit command / successful re-probe. Sidecar rewrites settings.json to remove or blank-out the key.

### 2.5.3 Dependencies this creates

The mechanism relies on **undocumented internals** of Claude Code:

- The settings file watcher must remain active.
- The state-change handler must continue calling `applyConfigEnvironmentVariables()` when `settings.env` changes.
- `getAnthropicClient()` must continue constructing a fresh client per request (not caching).
- `@anthropic-ai/sdk` must continue reading `ANTHROPIC_BASE_URL` at constructor time (vs. module load).

A Claude Code release that changes any of these breaks Token-Bay's consumer fallback. The plugin runs a **runtime compatibility probe** at startup — attempts a controlled settings.json mutation, verifies propagation via an echo ping to the sidecar's ccproxy, reverts — and disables consumer fallback with a clear diagnostic if the probe fails. Seeder mode is unaffected. Details at §5.3 step 2 and §11.

## 3. Module layout

```
plugin/
  bin/                       -- slash-command entry points
  sidecar/                   -- long-running background service
  hooks/                     -- Claude Code hook handlers
  tracker-client/            -- long-lived connection + RPCs
  identity/                  -- Ed25519 keypair mgmt, enrollment flow
  cc-bridge/                 -- Claude Code CLI bridge (/usage probe on consumer; full bridge on seeder — see §5.2 + §6.2)
  ccproxy/                   -- Anthropic-compatible HTTPS server on 127.0.0.1:PORT; receives redirected /v1/messages in network mode (§2.5, §5.4)
  settingsjson/              -- atomic read-modify-write of ~/.claude/settings.json + rollback journal for mid-session redirect (§2.5, §5.3, §5.5)
  rate-limit-detector/       -- parses Claude Code errors / output for 429 signals
  exhaustion-proof/          -- v1 two-signal (StopFailure + /usage) bundle assembly; v2 Claude-Code-attestation stub
  tunnel/                    -- consumer ↔ seeder P2P tunnel
  audit-log/                 -- append-only local log
  config/                    -- YAML parsing, validation, hot-reload
```

(Layout is illustrative.)

**Module note.** `cc-bridge/` is still used on both sides, but its role has narrowed on the consumer side: it only runs `/usage` probes now. The consumer's request body no longer flows through it — Claude Code's own live session handles that, redirected via `ccproxy` per §2.5.

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

### 4.2 Identity-challenge protocol (revised 2026-04-23)

**Account fingerprint source: `claude auth status --json`.** Claude Code ships a first-class non-interactive CLI subcommand that returns structured JSON (`src/cli/handlers/auth.ts:232-319`):

```json
{
  "loggedIn": true,
  "authMethod": "claude.ai",
  "apiProvider": "firstParty",
  "email": "user@example.com",
  "orgId": "cd2c6c26-fdea-44af-b14f-11e283737e33",
  "orgName": "...",
  "subscriptionType": "max"
}
```

The plugin uses `orgId` (a stable UUID assigned by Anthropic per Claude AI organization) as the account fingerprint. At enrollment:

1. Plugin generates an Ed25519 keypair locally.
2. Plugin runs `claude auth status --json` via a non-interactive exec (no subprocess environment inheritance beyond PATH).
3. Plugin requires `loggedIn: true`, `apiProvider == "firstParty"`, `authMethod == "claude.ai"`. On failure, enrollment aborts with a specific diagnostic (`internal/ccproxy.IsCompatible`).
4. Plugin derives `account_fingerprint = SHA-256(orgId)` and signs `enroll_preimage = hash("token-bay-enroll:v1" || nonce || account_fingerprint || identity_pubkey)` with its Ed25519 private key.
5. Tracker accepts the enrollment if the signature verifies. The tracker doesn't need to re-verify the `orgId` — the plugin's attestation is trust-on-first-use plus the L3 reputation subsystem's pattern detection over time.

**Why this is Q5(b) / option α, fully resolved.** Earlier drafts worried that Claude Code exposed no programmatic way to identify the user's account. `claude auth status --json` is that mechanism. No prompt-smuggling, no observational side-channel — a clean non-interactive CLI subcommand with a stable JSON schema.

**Caveats:**
- `orgId` is only present when `authMethod == "claude.ai"`. Users on raw API keys (`authMethod == "api_key"`) don't have an organization identifier. Token-Bay consumer-role enrollment is refused in that case (also independently refused by the ccproxy `IsCompatible` precondition — `/usage` probe requires OAuth anyway).
- If a user belongs to multiple Claude AI organizations and switches between them, `orgId` changes and the plugin detects a mismatch on next startup (surfaces a re-enrollment prompt). Not handled in v1; noted in §10.
- A user who rotates their Claude AI account onto the same `orgId` (e.g., password reset) retains their identity. A new org → new identity, starter-grant re-applied.

### 4.3 Key storage

- `~/.token-bay/identity.key` — Ed25519 private key, permissions `0600`.
- OS keychain integration optional.
- **No Anthropic API key is ever written by this plugin.** Anthropic authentication is entirely owned by Claude Code.

## 5. Consumer role — fallback pipeline

### 5.1 Trigger (two-stage)

In normal operation the plugin does **not** intercept Claude Code's HTTPS traffic — Claude Code talks to `api.anthropic.com` directly, and the plugin observes only via hooks. When the user opts into network fallback (§5.3), the plugin atomically redirects Claude Code's next `/v1/messages` call to its own sidecar via a settings.json mutation (§2.5). The plugin independently re-verifies the rate limit before triggering any redirect. Two activation paths:

- **Automatic.** A turn ends with `StopFailure{matcher: rate_limit, ...}`. The plugin runs the verification in §5.2, and on confirmed exhaustion, injects a one-turn confirmation to the user: "You've hit your Claude rate limit. Retry via Token-Bay network? (≈ N credits; your balance is M)."
- **Manual.** User runs `/token-bay fallback`. The plugin still runs the verification in §5.2; if `/usage` reports plenty of headroom, the plugin refuses with an informational message (the user's rate limit isn't actually exhausted; no reason to spend credits).

### 5.2 Rate-limit verification via `claude /usage`

Before routing anything to the network, the plugin invokes Claude Code's own `/usage` slash command to confirm current rate-limit state. This is a second, independent signal from Claude Code saying "yes, this account is at its limit right now" — not just reacting to a single failed turn which might be transient or race-conditioned.

**`/usage` is a TUI-only command.** `src/commands/usage/index.ts` declares it as `type: 'local-jsx'` — a React component (`src/components/Settings/Usage.tsx`) rendered via Ink. When Claude Code detects stdout is not a TTY, it bails out with a canned stub (`"You are currently using your subscription..."`) after ~5ms without calling the underlying `/api/oauth/usage` endpoint. The only way to capture the real rendered output is under a PTY.

**Invocation:** `claude /usage` spawned under a PTY. No `-p` flag (it forces non-TTY mode and the stub). No `--output-format json` (silently ignored in interactive mode anyway).

Go implementation uses `github.com/creack/pty`. Empirically observed output is ANSI-formatted TUI text containing one `<N>% used` token per rate-limit window (typically three: `Current session` / `Current week (all models)` / `Current week (Sonnet only)`). The tokens survive across ANSI escape boundaries for regex parsing.

**Latency: early-termination required.** A full TUI render takes ~15-20s (Claude Code cold-start + data fetch + steady-state refresh loop). The probe terminates as soon as ≥2 `% used` tokens are captured (`HardDeadline` default 8s) to bound the fallback decision cost.

**Classification thresholds:**
- Any token's percentage ≥ 95 → rate-limited (accounts for `Math.floor(utilization)` undercount).
- All tokens < 95 with ≥2 tokens captured → headroom.
- Fewer than 2 tokens captured → ambiguous.

**Verdict matrix:**

| `StopFailure` fired | `/usage` confirms exhaustion | Action |
|---|---|---|
| yes | yes | **Proceed with fallback** (this is the happy path). |
| yes | no | Ambiguous — `StopFailure` may have been a transient error or a stale race. Plugin offers retry-without-fallback first; falls back only if the user insists (manual `/token-bay fallback`). |
| no (manual invocation) | yes | Proceed with fallback. |
| no | no | Refuse fallback. Inform the user their local rate limit has headroom. |

The two-stage check materially improves the exhaustion-proof fidelity for v1 (see §5.4): the proof bundle carries both the `StopFailure` event and the `/usage` output, each independently signed by the consumer, giving the tracker + reputation system two correlated signals instead of one.

### 5.3 Enter network mode

On confirmed exhaustion (§5.2 verdict: proceed), the plugin transitions from passive-hook-observer to active-network-router via the following sequence:

1. **Collect the hook payload and `/usage` output.** The plugin's hook handler serializes both into a pending-fallback ticket and hands it to the sidecar over the local IPC socket. The ticket is what the sidecar will later use to assemble the exhaustion proof (§5.6).

2. **Sidecar runs the runtime compatibility probe** if not already done this session. The probe performs a controlled, reversible mutation to a dedicated field under `env` in settings.json (e.g. `env.TOKEN_BAY_PROBE`), then waits up to 2s for Claude Code to report back the new value via an echo ping against the sidecar's `ccproxy` health endpoint. On success the probe result is cached for the session (default TTL 1h). On failure, the sidecar aborts the fallback and surfaces: *"Your Claude Code version doesn't support Token-Bay's mid-session redirect. Update Claude Code, or use `/token-bay fallback-manual` (launches a new Claude Code with ANTHROPIC_BASE_URL pre-set)."* See §11.

3. **Sidecar records the pending ticket in its `network_mode` map** keyed by session id (from `SessionStart` hook) with TTL matching the exit criteria (§5.5).

4. **Sidecar atomically rewrites `~/.claude/settings.json`** via the `internal/settingsjson` module:
   - Read current `settings.json`.
   - Abort if `env.ANTHROPIC_BASE_URL` is already set to a non-Token-Bay value (user has their own proxy; layering is refused — *"Your settings redirect ANTHROPIC_BASE_URL elsewhere. Token-Bay cannot layer on top."*).
   - Abort if `CLAUDE_CODE_USE_BEDROCK`, `CLAUDE_CODE_USE_VERTEX`, or `CLAUDE_CODE_USE_FOUNDRY` is truthy in `env` — `ANTHROPIC_BASE_URL` is ignored under those providers (§11).
   - Merge `env.ANTHROPIC_BASE_URL = "http://127.0.0.1:PORT"`; preserve everything else verbatim.
   - Record the pre-fallback value of `env.ANTHROPIC_BASE_URL` (empty, absent, or whatever was there) in a rollback journal at `~/.token-bay/settings-rollback.json`.
   - Atomic write: write to `settings.json.token-bay-tmp-<pid>`, then `rename(2)` — POSIX atomic, survives crashes.

5. **Sidecar waits for confirmation that the redirect took effect.** Up to 2s for a health ping from Claude Code on the new base URL. On timeout, proceeds anyway but logs the ambiguity; the user's next real request is the authoritative signal.

6. **Plugin surfaces confirmation:** *"Network mode active — your next message will route through Token-Bay."*

### 5.4 Request lifecycle in network mode

While session X is flagged network-mode:

1. User types next message. Claude Code POSTs `/v1/messages` to `http://127.0.0.1:PORT` (sidecar's `ccproxy`).
2. Sidecar parses the request. Looks up the session in the `network_mode` map. Hit → route through network. Miss → treat as a stray; return an Anthropic-format 502 and log for diagnosis (this path should not fire in normal operation).
3. Sidecar computes `body_hash`, assembles the exhaustion proof from the cached ticket (§5.3 step 3 + §5.6), and builds the broker envelope: `(model, max_input_tokens, max_output_tokens, tier, body_hash, exhaustion_proof, consumer_sig, balance_proof)`.
4. Sidecar calls `tracker.broker_request(envelope)`. On `NO_CAPACITY`, returns an Anthropic-format 503 to Claude Code. Claude Code surfaces the failed turn; the plugin's `StopFailure` hook fires again with `matcher="server_error"` — which the plugin does NOT re-enter as a fallback trigger.
5. On seeder assignment: sidecar opens a QUIC tunnel to the seeder (NAT hole-punch via tracker STUN, TURN fallback — §7).
6. Sidecar streams the request body over the tunnel.
7. Seeder returns streaming chunks (seeder-side `claude -p --output-format stream-json`). Chunks are Anthropic-compatible SSE events.
8. Sidecar relays SSE chunks verbatim to Claude Code's open HTTP response. Claude Code renders them as normal streaming output.
9. On stream end: sidecar counter-signs the tracker's settlement request. Writes the turn to the audit log.
10. On mid-stream tunnel error: sidecar returns an Anthropic-format error to Claude Code. Same no-re-entry behavior as step 4.

**Important:** The exhaustion-proof bundle is assembled **in the sidecar at request time**, not in the hook at detection time. Freshness matters for the proof's validity (§5.6); assembly-at-use keeps the timestamps honest.

### 5.5 Exit network mode

Triggers:
- **Timer expires** — default 15 minutes from entering network mode; configurable as `consumer.network_mode_ttl` in `config.yaml`.
- **Explicit user command** — `/token-bay normal`. Also bound to `SIGUSR1` to the sidecar for power users.
- **Successful direct-Anthropic re-probe** (opt-in only; off by default) — sidecar periodically attempts a minimal direct call via `claude -p "/usage"` through the default endpoint. On 2xx, revert. Off by default because the re-probe itself consumes quota.

Exit sequence:
1. Sidecar reads the rollback journal at `~/.token-bay/settings-rollback.json`.
2. Sidecar atomically rewrites `settings.json`: if the pre-fallback value was absent, set `env.ANTHROPIC_BASE_URL = ""` (see caveat below); otherwise restore the prior string.
3. Sidecar removes the session from its `network_mode` map.
4. Plugin surfaces: *"Exited network mode — your next message goes directly to Anthropic."*

**Revert-semantics caveat.** `Object.assign(process.env, ...)` is additive-only. Removing `ANTHROPIC_BASE_URL` from settings.json's `env` block may NOT unset `process.env.ANTHROPIC_BASE_URL` in the running Claude Code — the property stays at the last-written value. Empirical verification is pending (see §10). Workarounds under investigation: set the key to empty string (which most HTTP clients interpret as "use default"), or explicitly set it to the known Anthropic default URL. If neither works reliably, exit requires a Claude Code restart, which defeats the point of mid-session redirect for the exit path. In that case, the `/token-bay normal` command would instruct the user to restart their Claude Code and indicate that auto-timer exits simply delete our entry from settings.json so that a subsequent restart is clean.

### 5.6 Exhaustion-proof capture

The proof bundle is **assembled in the sidecar at request time** (§5.4 step 3), not in the hook at detection time. The hook captures raw StopFailure and `/usage` data into a pending-fallback ticket (§5.3 step 1); the sidecar signs the bundle when Claude Code actually sends the redirected request. This keeps the proof's freshness timestamps honest — the proof's `captured_at` is seconds from the broker request, not minutes after the hook fired.

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

### Resolved in this revision

- ~~**Can a plugin inject streaming response content into the user's Claude Code conversation** in a way that looks native?~~ **Resolved:** no injection needed. In network mode, the sidecar's `ccproxy` is an Anthropic-compatible HTTPS endpoint that Claude Code sees as its native API. Claude Code renders the response via its usual streaming path; the sidecar is invisible to the conversation layer.

### New — mid-session redirect (§2.5)

- **Revert semantics for `process.env` removal.** `applyConfigEnvironmentVariables()` uses `Object.assign`, which is additive. Removing `ANTHROPIC_BASE_URL` from settings.json's `env` may not clear `process.env.ANTHROPIC_BASE_URL` in the running Claude Code. Needs empirical verification. Candidate fallbacks in order of preference: set key to `""` (empty string), set key to Anthropic's default URL, surface a "please restart Claude Code" message to the user.
- **Upstream stability of the mechanism.** The chain we depend on (settings watcher → state handler → `applyConfigEnvironmentVariables` → per-request client construction → SDK reading env at constructor) is undocumented. Any Claude Code release could change it. Runtime probe at plugin startup detects breakage (§5.3 step 2); but continuous monitoring in production is also needed.
- **Race with user's manual edits to `settings.json`.** If the user edits settings.json while the sidecar is mid-atomic-write, one of the writes may be clobbered. Atomic rename + last-writer-wins + a post-write re-read reconciliation reduces but doesn't eliminate the race. Consider a lock file (`settings.json.lock`) though that introduces its own cleanup concerns.
- **Propagation-delay vs user-typing-speed race.** Between plugin consent and the next user prompt, the watcher must see settings.json, AppState must update, `process.env` must be mutated — all before Claude Code builds its next Anthropic client. ~50ms for the watcher is typical; real-world human typing is much slower. But if the user pastes a pre-prepared prompt and hits Enter immediately, they could theoretically beat the propagation. Mitigation: during §5.3 step 5 the sidecar waits for a health-ping from Claude Code before surfacing the "Network mode active" message. The user isn't prompted to type until propagation has been observed.
- **Bedrock / Vertex / Foundry users.** `ANTHROPIC_BASE_URL` is ignored when `CLAUDE_CODE_USE_BEDROCK`, `CLAUDE_CODE_USE_VERTEX`, or `CLAUDE_CODE_USE_FOUNDRY` is truthy. Plugin detects at enrollment and refuses consumer-role activation with a clear message. Seeder role still works (seeder's own Claude Code can use any provider it likes when serving).
- **Fallback-manual UX** for when the runtime probe fails. Launching a new Claude Code terminal pre-configured with `ANTHROPIC_BASE_URL` set would work, but loses session context. Investigation needed: can Claude Code be launched with `--resume <id>` or equivalent to restore the prior session? If so, fallback-manual becomes a reasonable degraded-mode UX.

### Carried from prior revision

- **Are the tool-disabling flags airtight across every Claude Code release?** The seeder bridge's entire safety argument rests on `--disallowedTools "*" + --mcp-config /dev/null + empty hooks` being honored. Conformance test in §12 must re-validate on every Claude Code upgrade. Any regression is a CVE-class issue and must gate the seeder role.
- ~~**What does Claude Code expose for identity verification?**~~ **Resolved (2026-04-23):** `claude auth status --json` (a first-class CLI subcommand at `src/cli/handlers/auth.ts:232-319`) returns structured JSON including a stable `orgId` UUID when `authMethod == "claude.ai"`. Plugin uses `SHA-256(orgId)` as the account fingerprint. See §4.2. Multi-org users and org switches are a v1 caveat (re-enrollment prompt on mismatch; handled in `internal/identity` feature plan).
- **Exact shape of the `StopFailure{matcher:rate_limit}` payload.** Determines the structured fields available for §5.6. Token-Bay's spec assumes at minimum a timestamp and an error description; richer payloads (retry-after, reset window) feed better proof fidelity and smarter UX.
- ~~**Exact output format of `claude -p "/usage"`.**~~ **Resolved (2026-04-23):** `/usage` is `type: 'local-jsx'` — a React TUI component with no non-TTY representation. Source-code investigation confirmed any non-PTY invocation returns a hardcoded stub. Plugin now spawns `claude /usage` under a PTY via `github.com/creack/pty` and regex-scans the rendered TUI for `<N>% used` tokens. See §5.2 and `docs/superpowers/plans/2026-04-23-plugin-ratelimit.md` for the parser design.
- **Streaming support end-to-end.** If the seeder bridge doesn't stream, the network degrades to request-response-only. Affects seeder capabilities advertised to the tracker.
- **Multi-session users.** Two Claude Code windows open, both hit a limit. Both redirect settings.json — but settings.json is a single shared file. Plugin needs either: one sidecar per Claude Code process (more complex), or one shared sidecar that multiplexes both redirected sessions via path or header discrimination. Leaning shared-sidecar; the session id in the StopFailure hook and the `X-Claude-Code-Session-Id` header seen by ccproxy (Claude Code sends it) together let one sidecar route correctly.
- **Windows story.** `bubblewrap` and `firejail` are Linux-only. Windows seeder sandboxing needs WSL2 or a Windows-native sandbox. v1 may be Linux-and-macOS only for seeder; consumer side (which doesn't need a sandbox) could still work on Windows.

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
| Runtime compatibility probe fails at startup (§5.3 step 2) | Consumer fallback disabled for this session. Diagnostic surfaced: likely cause + remediation. Seeder role unaffected. Probe re-runs on next plugin launch; succeeds again when user updates Claude Code. |
| User has `CLAUDE_CODE_USE_BEDROCK` / `VERTEX` / `FOUNDRY` set | Consumer role refuses activation at enrollment. Explanation: `ANTHROPIC_BASE_URL` is ignored under these providers. Seeder role still works on these providers. |
| `~/.claude/settings.json` is not writable (permission, symlink to read-only location, etc.) | Consumer fallback unavailable. Specific error: which path, what error code. Seeder role unaffected. |
| Settings.json write races with user's manual edit | Last-writer-wins. Sidecar re-reads after write and logs if its change was clobbered. Surfaces a warning to the user: "Your manual edit to settings.json was preserved; Token-Bay was not activated. Try again." |
| User pastes a prompt faster than the settings watcher can propagate | Mitigated by §5.3 step 5 (sidecar waits for health-ping before showing "active" message to user). If the user bypasses the UX and sends a prompt during the ~50ms window, the first post-consent prompt still goes to Anthropic and re-triggers StopFailure; second attempt succeeds through the sidecar. Not a data-loss event, just a retry. |
| `process.env.ANTHROPIC_BASE_URL` does not unset on revert (Object.assign-is-additive bug — §5.5 caveat) | Per-session persistence of the redirect until Claude Code restart. Sidecar surfaces a clear notice on `/token-bay normal`. v1 workaround: set the env var to empty string; if that fails, instruct restart. |

## 12. Acceptance criteria

- Plugin is installable via `/plugin` in Claude Code. Enrollment completes without the plugin ever handling an Anthropic API key.
- **Runtime compatibility probe passes on a current Claude Code release at plugin install time**, and a clean diagnostic is produced when run against a Claude Code build that has broken the settings-watcher / per-request-client chain.
- When the user's Claude Code hits a rate limit and consents to fallback, the sidecar's settings.json mutation propagates and the **user's next prompt routes through Token-Bay within 2 seconds, without Claude Code restart or context loss.**
- After `/token-bay normal` (or timer expiry), the user's subsequent prompts route directly to Anthropic — settings.json is restored to its pre-fallback state modulo the Object.assign caveat (§5.5).
- Atomic settings.json writes: if the plugin process is killed mid-write, settings.json is never left in a partial state (the `rename(2)` atomicity guarantees this; verified via fault-injection test).
- Seeder mode executes forwarded requests through `claude -p` with the flag set in §6.2; a **bridge conformance test** drives a suite of adversarial prompts (shell commands, file reads, URL fetches, MCP invocations, hook-triggering content) against the configured `claude` binary and asserts zero side effects on the seeder's filesystem, processes, or network. The test runs at plugin startup, on every Claude Code version change, and in CI.
- Switching idle policy takes effect within one heartbeat.
- Audit log is queryable via `/token-bay logs`.
- `/token-bay balance` returns a fresh signed snapshot.
- Plugin recovers from tracker disconnection and resumes seeder-mode within one retry interval.
- **Negative test:** intentional attempt to read `~/.token-bay/` or `~/.claude/credentials*` via a maliciously-crafted forwarded prompt is blocked by the tool-disabling flags and produces no side effect. Sandbox hardening is optional belt-and-braces, not required to pass this test.

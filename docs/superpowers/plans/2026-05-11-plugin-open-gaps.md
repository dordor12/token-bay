# Plugin — open gaps

| Field | Value |
|---|---|
| Date | 2026-05-11 |
| Scope | Outstanding wiring / correctness / config-consumption gaps in the `plugin/` module after rebase onto `main` at `e80e629` |
| Method | Cross-checked specs in `docs/superpowers/specs/plugin/` (plugin design, envelopebuilder, trackerclient, identity, ccproxy-network, ssetranslate, config-auditlog) against `plugin/` source; deep-dive audits of cmd-layer composition, internal subsystems, and Claude Code plugin surfaces |

## Recently closed by #43 (for context)

PR #43 ("wire missing periodic routines and consumerflow supervisor") landed the entire consumer-side composition:

| Gap | Evidence |
|---|---|
| `consumerflow.Coordinator` constructed in cmd | `plugin/cmd/token-bay-sidecar/consumerflow_cmd.go` (158 lines) + `run_cmd.go:142` |
| `BrokerClient` adapter (mirrored types) | `consumerflowTracker` adapter mapping `trackerclient.BrokerResult` → `consumerflow.BrokerResult` |
| `SessionModeStore` instantiated and shared between ccproxy and consumerflow | `run_cmd.go` |
| `settingsjson.Store` wired (`NewStore` + atomic enter/exit) | via `consumerflowTracker` adapter |
| `ProofBuilder` / `EnvelopeBuilder` constructed and injected | `exhaustionproofbuilder.Builder`, `envelopebuilder.Builder` + `shared/signing.SignEnvelope` signer adapter |
| `UsageProber` wired | `ratelimit.NewClaudePTYProbeRunner()` |
| `SidecarURLFunc` resolves bound `ccproxy` URL post-Start | `atomic.Pointer[sidecar.App]`-backed callback |
| `Coordinator.Run` TTL-reap goroutine reachable | `sidecar.go:144` `if a.deps.ConsumerFlow != nil` now fires |

The header of the PR explicitly defers two items: **(P1) hook subprocess → Coordinator IPC** and **(P2) `plugin.json` hook/slash-command declarations**.

## Open — blockers (consumer side still unreachable from Claude Code)

### P1 — Hook subprocess entry point missing

- **Where:** No file under `plugin/cmd/token-bay-sidecar/` reads stdin or invokes `hooks.Dispatcher.Handle(ctx, eventName, in, out)`. `hooks.Dispatcher` is fully implemented (`plugin/internal/hooks/`); the Claude Code hook contract expects a subprocess that reads the event JSON from stdin and emits a Response on stdout.
- **Symptom:** `StopFailure(rate_limit)` from Claude Code has no path into Go. The wired `Coordinator` never receives events.
- **Plan:** Add a `hook` subcommand under `cmd/token-bay-sidecar/` (or a separate `cmd/token-bay-hook/`) that:
  - Connects to the running sidecar (Unix socket under `~/.token-bay/sidecar.sock`).
  - Forwards the parsed event over the socket to a sidecar handler that dispatches into `consumerflow.Coordinator`.
  - Returns the Response JSON within Claude Code's hook timeout budget.
- **Open question:** Single binary with a `hook` subcommand vs separate small binary. The single-binary path is consistent with `plugin/CLAUDE.md` rule 5 ("consumer and seeder roles share one binary").

### P2 — `plugin.json` declarations empty; commands/hooks dirs empty

- **Where:** `plugin/.claude-plugin/plugin.json` has `"commands": []`, `"hooks": []`. `plugin/commands/` and `plugin/hooks/` each contain only `.gitkeep`.
- **Plan:** After P1 lands, populate:
  - `commands/`: thin wrappers for `/token-bay enroll|status|balance|fallback|seed|logs` invoking `token-bay-sidecar <subcommand>`.
  - `hooks/`: hook script(s) for `StopFailure` (matcher `rate_limit`), `UserPromptSubmit`, `SessionStart`, `SessionEnd` — each just spawns the P1 hook entry point.
  - `plugin.json`: register both arrays.

### P3 — Seeder side: `NopAcceptor` still in place

- **Where:** `plugin/cmd/token-bay-sidecar/run_cmd.go:263` hard-codes `Acceptor: seederflow.NopAcceptor{}`.
- **Symptom:** Seeder advertises availability but the acceptor never returns a connection. Consumers can never reach a seeder.
- **Blocked on:** Wire-format gap acknowledged in `plugin/internal/seederflow/doc.go` (tunnel metadata).
- **Plan:** Land the tunnel-metadata wire-format amendment, then replace with a real `tunnel.Listener` driven by the QUIC transport.

## Open — correctness / safety

### P4 — Runtime compatibility probe (plugin spec §5.3 step 2 / §11)

- **Where:** `consumerflow.Deps.RuntimeProbeOK` exists at `plugin/internal/consumerflow/deps.go:171`; no cmd-layer code populates it.
- **Symptom:** Without the probe, a Claude Code upstream change that breaks the settings-watcher chain disables consumer fallback silently.
- **Plan:** At sidecar startup, atomically mutate `~/.claude/settings.json` to point at a `ccproxy` echo endpoint, await an echo within a small budget, revert. Set `RuntimeProbeOK` accordingly. Refuse `OnStopFailure` if false with a §11 diagnostic.

### P5 — `trackerclient.balanceCache.invalidate` still unused

- **Where:** `plugin/internal/trackerclient/balance_cache.go:84` — `//nolint:unused`.
- **Symptom:** On connection drop, stale balance snapshots from before the disconnect remain cached.
- **Plan:** Call `b.cache.invalidate()` in `supervisor.handleDrop` (reconnect path). Add a small ticker to refresh proactively when remaining lifetime falls below `BalanceRefreshHeadroom` (spec §7 line 87 acceptance criterion).

### P6 — ccproxy `NetworkRouter` has no `SessionModeStore` reference

- **Where:** `plugin/internal/ccproxy/server.go:55-56` constructs `Network: &NetworkRouter{Dialer: NewTunnelDialer()}` with no Sessions field. `SessionModeStore.GetMode()` populates `EntryMetadata` (ephemeral key, seeder addr, pubkey) on `EnterNetworkMode`; the router needs to read it on each request.
- **Plan:** Add `Sessions ccproxy.SessionModeStore` to `NetworkRouter`; `ServeHTTP` reads the session ID from the request (header or path), looks up `EntryMetadata`, and passes ephemeral identity into `Dialer.Dial`.

### P7 — SSE translator not wired on consumer side

- **Where:** `plugin/internal/ssetranslate/` is instantiated only in `plugin/internal/seederflow/serve.go`. The consumer side (`ccproxy.NetworkRouter`) currently does raw `io.Copy` on the seeder's response body.
- **Open question:** Verify whether the seeder converts to SSE before sending. If yes, consumer-side translation is correctly absent; if no, the consumer sees stream-json that Claude Code can't parse.
- **Plan:** Confirm direction in the ssetranslate spec; if asymmetric, instantiate the translator on the consumer response path.

### P13 — Exhaustion proof built at hook time, cached for the session

- **Where:** `plugin/internal/consumerflow/coordinator.go:183`
- **Symptom:** Proof timestamp `now - stop_failure.at` must be ≤ 60s per spec §5.6 + exhaustion-proof §3.2. With default 15-min `NetworkModeTTL`, the cached proof goes stale by minute 1+.
- **Plan:** Rebuild proof in `ccproxy.NetworkRouter` (or via a Coordinator callback) when each redirected `/v1/messages` arrives; or reduce TTL aggressively and rebuild on every `BrokerRequest`.

### P14 — ccproxy `PassThroughRouter` Authorization header preservation

- **Where:** `plugin/internal/ccproxy/router.go` uses `httputil.ReverseProxy` defaults. Plugin `CLAUDE.md` rule 3 mandates forwarding `Authorization` *verbatim* — never parsing, logging, or mutating.
- **Plan:** Replace the default `Director` with an explicit one that preserves the raw `Authorization` header byte-for-byte; add a test that asserts no header rewriting under standard `ReverseProxy` behavior.

## Open — minor / polish

| ID | Gap | Location | Plan |
|---|---|---|---|
| P8 | Audit log rotation manual-only | `plugin/internal/auditlog/Logger.Rotate` defined; no scheduler | Add a daily ticker in `sidecar.Run`; rotate by file (per repo CLAUDE.md rule 5) |
| P9 | Config hot-reload (`// TODO §9`) | `plugin/internal/config/doc.go:13` | SIGHUP listener invokes `config.Load()`; emit only "non-structural" fields to subsystems |
| P10 | Bedrock / Vertex / Foundry enrollment rejection | `plugin/cmd/token-bay-sidecar/enroll_cmd.go` | Check `CLAUDE_CODE_USE_{BEDROCK,VERTEX,FOUNDRY}`; refuse with a clear §10 diagnostic |
| P11 | Settings.json revert: no re-read confirmation | `plugin/internal/settingsjson/` (exit path) | After rewriting settings.json on exit, send a ccproxy probe; if it still routes to network mode, surface a "restart Claude Code" message |
| P12 | Config fields `PrivacyTier`, `MaxSpendPerHour`, `Consumer.NetworkModeTTL` consumption | `plugin/cmd/token-bay-sidecar/run_cmd.go` | Spot-check after #43 — confirm all three flow into `consumerflow.Deps` / envelope construction |

## Priority ladder

1. **P1 + P2** — finish what #43 deferred: hook subprocess entry + plugin.json declarations. Without these, the entire consumer fallback flow remains unreachable from Claude Code.
2. **P14** — ccproxy Authorization preservation (CLAUDE.md rule 3 is non-negotiable).
3. **P13** — request-time proof rebuild (spec §5.6 staleness).
4. **P4** — runtime compatibility probe (safety net for Claude Code upstream changes).
5. **P5** — `balanceCache.invalidate` on reconnect + background refresher.
6. **P6** — `NetworkRouter ↔ SessionModeStore` cross-package wiring.
7. **P3** — seeder `tunnel.Listener` (blocked on wire-format amendment).
8. **P7** — verify and wire SSE translator direction.
9. **P8 / P9 / P10 / P11 / P12** — polish.

# plugin/internal/consumerflow — Development Context

## What this is

The consumer-side fallback orchestrator. Composes the leaf packages
(`hooks`, `ratelimit`, `exhaustionproofbuilder`, `envelopebuilder`,
`settingsjson`, `ccproxy.SessionModeStore`, `trackerclient`,
`auditlog`) into the §5 consumer flow. It is the single owner of the
state-machine that takes a session from "Claude Code observed a
StopFailure(rate_limit)" through "ccproxy is in network mode and
settings.json points at the sidecar".

Authoritative spec: `docs/superpowers/specs/plugin/2026-04-22-plugin-design.md` §5
+ §11 failure modes.

## Public API

| Symbol | Purpose |
|---|---|
| `Deps` | Caller-supplied collaborators + config knobs |
| `Deps.Validate() error` | Required-field checks; chained off `ErrInvalidDeps` |
| `New(Deps) (*Coordinator, error)` | Construct; no goroutines start |
| `(*Coordinator).Run(ctx) error` | Periodic TTL reaper; blocks until ctx cancel |
| `(*Coordinator).OnStopFailure(ctx, *ratelimit.StopFailurePayload) error` | hooks.Sink |
| `(*Coordinator).OnSessionStart(ctx, *hooks.SessionStartPayload) error` | hooks.Sink |
| `(*Coordinator).OnSessionEnd(ctx, *hooks.SessionEndPayload) error` | hooks.Sink |
| `(*Coordinator).OnUserPromptSubmit(ctx, *hooks.UserPromptSubmitPayload) error` | hooks.Sink |

## Source layout

| File | Purpose |
|---|---|
| `doc.go` | Package documentation |
| `errors.go` | `ErrInvalidDeps` |
| `deps.go` | `Deps`, collaborator interfaces, mirrored Broker types |
| `coordinator.go` | `Coordinator`, the state machine, Run, Sink methods |
| `coordinator_test.go` | Integration tests with in-memory fakes |

## Non-negotiable rules

1. **No filesystem I/O in this package.** All disk-resident state
   (settings.json + rollback journal, audit log) is opened by the cmd
   layer and handed in via Deps. The package's tests use real
   `settingsjson.Store` against a `t.TempDir()` because `settingsjson`
   itself encapsulates filesystem boundaries — but the orchestrator
   never calls `os.*` directly.
2. **Errors never bubble to the host.** Every Sink method returns nil
   on internal failure. Refusal modes audit and return; the host
   Claude Code's hook subprocess always sees a clean empty Response.
3. **Audit on every decision.** Each branch of `handleRateLimit` ends
   with either an `auditlog.LogConsumer(ConsumerRecord{...})` or a
   refusal record. Tests assert on those records.
4. **§11 coverage.** Every "Failure" row in plugin spec §11 that
   mentions consumer-side behavior has a refusal branch in
   `handleRateLimit` and a corresponding test in `coordinator_test.go`.
5. **Mirrored Broker types.** `BrokerResult` / `SeederAssignment` /
   etc. are mirrored locally so this package has zero compile-time
   dependency on `trackerclient`'s exported result types. The cmd
   layer adapts `*trackerclient.Client` to the local
   `BrokerClient` interface via a small adapter.

## Things that look surprising and aren't bugs

- **`Now: time.Now`, not an injected fake clock.** ccproxy's
  `SessionModeStore.GetMode` compares `entry.ExpiresAt` against
  *real* `time.Now()` (it is shared infrastructure, not part of this
  package). Using a fixed-Unix Now in tests makes freshly-entered
  sessions look already-expired. Tests that need deterministic
  proof timestamps inject a fixed Now into ProofBuilder/EnvelopeBuilder
  directly.
- **`SidecarURLFunc` is a callback, not a string.** ccproxy may bind
  to `:0` and the resolved port is only known after Start. The
  Coordinator resolves the URL lazily at use time.
- **BodyHash is a 32-byte placeholder.** The actual request body
  isn't available at hook time (spec §5.4 step 3 is the future-work
  reconciliation point). v1 fills BodyHash with random bytes — the
  reservation token from the broker is what binds the assignment to
  the upcoming request.
- **EphemeralKeyGen is per-fallback.** A fresh consumer-side
  ed25519 keypair is generated on every fallback entry. The private
  key lives in the SessionModeStore entry; ccproxy's NetworkRouter
  passes it into `tunnel.Dial`.
- **Settings.EnterNetworkMode runs *after* SessionStore.EnterNetworkMode.**
  If settings.json fails to write, we roll the SessionModeStore back.
  Order matters because the redirected request can only arrive on
  ccproxy after settings.json has propagated; entering ccproxy's
  network mode first is racy-safe (we expect zero traffic before the
  watcher fires).

## When this package changes, also touch

- `plugin/internal/sidecar/deps.go` — the optional `ConsumerFlow`
  field is the single integration seam.
- `plugin/internal/sidecar/sidecar.go` — the goroutine launch in
  `Run` mirrors the `Janitor` pattern.
- The cmd layer (separate plan) — owns construction of `Deps`, the
  runtime compatibility probe, and the BrokerClient adapter.

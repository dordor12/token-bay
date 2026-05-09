# plugin/internal/sidecar — Development Context

## What this is

The long-lived supervisor for the `token-bay-sidecar` process. Composes the
plugin's subsystems (identity, auditlog, trackerclient, ccproxy) into one
`App` with a Run/Close lifecycle. No business logic — every concrete
behavior (slash-command handlers, hook ingestion, network-mode coordination,
offer/settlement push handlers) lives in its own package and is wired in
via a follow-on plan.

Authoritative spec: `docs/superpowers/specs/plugin/2026-04-22-plugin-design.md` §2.1, §3.

## Public API

| Symbol | Purpose |
|---|---|
| `Deps` | Caller-supplied inputs (signer, audit log, tracker endpoints, ccproxy bind). |
| `Deps.Validate() error` | Required-field checks; chained off `ErrInvalidDeps`. |
| `New(Deps) (*App, error)` | Builds subsystems but does not start them. |
| `(*App).Run(ctx) error` | Starts subsystems, blocks until ctx-cancel, then ordered shutdown. |
| `(*App).Status() Status` | Snapshot for `/token-bay status` (eventually IPC-exposed). |
| `(*App).Close() error` | Releases resources without running shutdown (used pre-Run). |

## Source layout

| File | Purpose |
|---|---|
| `doc.go` | Package documentation |
| `errors.go` | `ErrInvalidDeps`, `ErrAlreadyStarted`, `ErrClosed` |
| `deps.go` | `Deps` + `Validate` |
| `sidecar.go` | `App`, `New`, `Run`, `Close` |
| `status.go` | `Status` + `(*App).Status` |

One test file per source file.

## Non-negotiable rules

1. **No filesystem I/O.** All disk-resident state (config file, identity files,
   audit log, settings.json) is opened by the cmd layer and handed in via
   `Deps`. The supervisor must remain trivially testable without `t.TempDir()`
   plumbing inside the package itself.
2. **No business logic.** Network-mode coordination, offer/settlement
   handling, slash-command routing, and hook ingestion each have their own
   package. Adding a method here that does anything other than start, stop,
   or observe is a layering smell.
3. **Subsystem startup is synchronous.** A bind error in `ccproxy.Start` must
   surface from `Run` *before* it blocks on ctx. Background loops own their
   own reconnect; a mid-run subsystem failure does not abort the supervisor.
4. **Shutdown order is ccproxy → trackerclient → auditlog.** ccproxy first
   so no inbound traffic; tracker next so push acceptors drain; auditlog
   last because shutdown writes its own final entry (when the future hook
   owner adds one).
5. **`Run` is single-shot.** `ErrAlreadyStarted` on the second call.
   Restart = new `App`.

## Things that look surprising and aren't bugs

- `Close` *before* `Run` releases the audit log handle (the only caller-owned
  fd not transferred via `Run`'s shutdown path). After `Run` it is a no-op
  because shutdown already ran.
- `TrackerClientOverrides` is a callback, not a Config copy. Lets the cmd
  layer flip a single timeout in tests without re-declaring the whole
  `trackerclient.Config`.
- `Deps.AuditLog` is opened by the caller because audit-log paths come from
  config and the caller already touched config; threading it through the
  supervisor would just relay the same value.
- `OfferHandler` and `SettlementHandler` are not in `Deps` yet —
  `trackerclient` tolerates nil for both, and the seeder/consumer push
  paths land in their own feature plans.
- `closeIgnoreServerClosed` swallows `http.ErrServerClosed` from
  `ccproxy.Close`. The ccproxy `Start` registers its own ctx-cancel
  goroutine that calls `Close`, so the supervisor's explicit close races
  with that and may see an already-shut-down server. Both calls are
  no-ops post-shutdown except for the sentinel error.

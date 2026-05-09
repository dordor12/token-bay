# tracker/internal/session — Development Context

## What this is

Per-request lifecycle state shared by `tracker/internal/broker`'s
selection (`*Broker`) and settlement (`*Settlement`) subsystems.
Two stores (`Inflight`, `Reservations`) bundled into a `Manager`,
plus a `SweepExpiredAndFail` helper that the broker's reaper drives.

Authoritative spec: `docs/superpowers/specs/tracker/2026-05-07-tracker-session-design.md`.

## Non-negotiable rules

1. **No registry/ledger/admission/push imports.** Session is state,
   not orchestration. The broker is the only legitimate consumer.
2. **No goroutines.** Timers and tickers live in broker; session
   exposes synchronous helpers only.
3. **Race-clean is mandatory.** `go test -race` runs by default
   (see tracker/CLAUDE.md always-`-race` list).
4. **CAS transitions.** State changes go through `Inflight.Transition`
   only — direct field mutation is a bug.

## Tech stack

- Go 1.25, stdlib `sync`, `crypto/ed25519`, `time`
- Imports `shared/ids` and `shared/proto` (only for `EnvelopeBody`)
- Tests via `testify`

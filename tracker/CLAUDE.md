# tracker — Development Context

## What this is

The Token-Bay regional coordination server. One process per region. Serves consumers (plugins in consumer role) requesting fallback routing, seeders (plugins in seeder role) registering availability, and peer trackers participating in federation.

Authoritative spec: `docs/superpowers/specs/tracker/2026-04-22-tracker-design.md`. Related:

- `docs/superpowers/specs/ledger/` — ledger format and semantics
- `docs/superpowers/specs/federation/` — peer protocol
- `docs/superpowers/specs/reputation/` — abuse detection

## Non-negotiable rules (tracker-specific)

1. **Tracker is in the control path, not the data path** (plugin spec §1.3). The seeder↔consumer tunnel is peer-to-peer via STUN hole-punching, with tracker TURN relay only as a fallback. Do not add data-path features that route bulk traffic through the tracker by default.
2. **Append-only ledger.** Entries are never rewritten, never deleted. Merkle-root history is archived forever. Equivocation is detected by peers — you cannot cover it up locally.
3. **Tracker key compromise is a recoverable incident, not a rewrite opportunity.** Rotation re-signs the chain tip; the history stays intact.
4. **Reputation actions are locally scoped.** A `FROZEN` state for an identity is authoritative in its home region only. Federation-gossiped revocations are advisory to peer regions.
5. **No Anthropic API key is ever stored on the tracker.** The tracker never talks to Anthropic directly — seeders do, through their own Claude Code bridges. The tracker validates ledger entries that reference Anthropic-sourced work but does not authenticate to Anthropic.
6. **Signed balance snapshots have a 10-minute TTL.** No longer. Caching a snapshot beyond its expiry is a protocol violation.

## Tech stack

- Go 1.23+
- QUIC via `quic-go`
- SQLite via `modernc.org/sqlite` (pure-Go, no cgo)
- CLI via `spf13/cobra`, config via `spf13/viper` + YAML
- Logging via `rs/zerolog`
- Metrics via `prometheus/client_golang`
- Tests via stdlib + `testify`
- Imports `shared/` for wire formats and crypto helpers

## Project layout

- `cmd/token-bay-tracker/` — thin entry point
- `internal/<module>/` — server, api, session, registry, broker, ledger, federation, reputation, stunturn, admin, config, metrics
- `test/e2e/` — end-to-end with stubbed plugin clients
- `test/integration/` — cross-module integration using the real SQLite backend
- `deployments/` — systemd unit, Dockerfile

## Commands (run from `tracker/`)

| Command | Effect |
|---|---|
| `make test` | Unit + integration with race detector |
| `make lint` | `golangci-lint run ./...` |
| `make build` | Build `bin/token-bay-tracker` |
| `make run-local` | Run the tracker locally with a generated keypair and SQLite file under `.token-bay-local/` |
| `make check` | `test` + `lint` |
| `make docker` | Build the Docker image |

## Development workflow

Same repo-wide TDD discipline. Additional notes:

- Tests that hit the SQLite backend use a tempdir-backed DB per test (`t.TempDir()` + in-memory `file::memory:?cache=shared`). Do not share a DB file across tests.
- `internal/broker`, `internal/federation`, and `internal/admission` are the most concurrent modules. Run `go test -race` by default; flakes there are always real bugs.
- Use the Go workspace: a change in `shared/` that touches a proto struct will affect the tracker's compilation immediately; fix both sides in the same commit.

## Things that look surprising and aren't bugs

- The tracker does not know Anthropic's rate limits. It trusts the seeder's self-reported usage and catches systematic dishonesty via `internal/reputation` (see reputation spec).
- The ledger balance projection is a cache. The chain is the source of truth; the projection is rebuildable by replay.
- SQLite is the v1 backend. Yes, for a "distributed system." Regional trackers are single-process; SQLite is enough. Switching to a bigger DB happens if/when a region outgrows one process.
- **Wire-format types live in `shared/proto/`, not in the tracker package named after them.** `tracker/internal/ledger/entry` owns the operations on `Entry` (Kind enum, Hash, builders, VerifyAll), but the `Entry` / `EntryBody` structs themselves are in `shared/proto/ledger.proto`. Same convention everywhere a struct crosses the network: federation streams entries, peers exchange transfer proofs, so the wire form goes in `shared/`. Don't be misled by a tracker package's name — read its `doc.go` to confirm where the type lives.

# Token-Bay — Repository Context

## What this is

**Educational / research exercise** in distributed-systems design. Token-Bay explores a federated P2P network for sharing Claude Code rate-limit capacity between consenting users. It is **not production software targeting live Anthropic accounts** — credential sharing would violate Anthropic's terms. The design explores the *protocol patterns* (federated trackers, signed reputation, P2P tunnels) using Claude rate-limit capacity as the notional payload.

## Repo layout

Monorepo with three Go modules linked by `go.work`:

- `plugin/` — Claude Code plugin (consumer + seeder roles). Has `plugin/CLAUDE.md`.
- `tracker/` — regional coordination server. Houses broker, ledger, federation, reputation as internal modules. Has `tracker/CLAUDE.md`.
- `shared/` — shared Go library. Wire formats, crypto helpers, common types. Has `shared/CLAUDE.md`.

Specs live at `docs/superpowers/specs/`. Plans at `docs/superpowers/plans/`. Always read the relevant subsystem spec before making non-trivial changes in a component.

## Non-negotiable repo-wide rules

1. **No third-party crypto.** Ed25519 uses stdlib `crypto/ed25519`. No libsodium, no OpenSSL bindings. Applies across all three modules.
2. **No Anthropic API key handling in code.** Not in plugin, not in tracker, not in shared. The architecture eliminates the need entirely.
3. **Shared types live in `shared/`.** Never duplicate a wire-format struct between `plugin/` and `tracker/`. If both sides need it, it's a `shared/` contribution.
4. **Breaking changes to `shared/` are coordinated.** A PR that modifies `shared/` must update callers in `plugin/` and `tracker/` in the same PR.
5. **Append-only audit logs and ledger entries are permanent.** No rewrite, no truncate — only rotation by file.

## Working across modules

Run `go work sync` after pulling to sync the workspace. The `go.work` file is committed; local-only overrides go in `go.work.local` (ignored by git — not created by default).

## Commands you'll use daily (from repo root)

| Command | Effect |
|---|---|
| `make test` | Runs `go test -race ./...` in each module |
| `make lint` | `golangci-lint run ./...` across all modules |
| `make build` | Builds all component binaries |
| `make check` | `test` + `lint` |

Module-specific commands live in each component's Makefile — e.g. `make -C plugin conformance` or `make -C tracker run-local`.

## Development workflow

TDD is the discipline across the entire repo — failing test first, green, refactor, commit. One conventional-commit per red-green cycle (`feat:`, `fix:`, `test:`, `refactor:`, `docs:`, `chore:`, `ci:`).

Commits should be small and should not cross component boundaries unless necessary. A change to `shared/` that requires updates in `plugin/` and `tracker/` is the legitimate exception and goes in a single cross-cutting commit.

## Submitting a PR

Open a PR only after **all session tasks are finished** and **`make check` is green locally** (`make test` + `make lint`). Then use the `gh` CLI:

1. Confirm local state is clean and the branch is pushed:
   ```
   git status
   git push -u origin HEAD
   ```
2. Create the PR against `main` with `gh`:
   ```
   gh pr create --base main --fill
   ```
   Use `--title` / `--body` (HEREDOC) when the auto-filled message isn't sufficient. Keep titles under ~70 chars; put detail in the body.
3. Watch CI until it finishes:
   ```
   gh pr checks --watch
   ```
4. **Merge only when CI is green.** Confirm via `gh pr checks` (all checks `pass`) before merging:
   ```
   gh pr merge --squash --delete-branch
   ```
   Never merge with failing or pending checks, and never bypass branch protections (`--admin`, `--no-verify`) without explicit user approval.

## Where to ask questions

Open a GitHub issue. Architecture discussion on the issue; the spec is the source of truth. If the spec is wrong or ambiguous, fix it in a PR alongside any code change.

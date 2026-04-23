# plugin — Development Context

## What this is

The Token-Bay Claude Code plugin. Runs on end-user machines. Two roles:

- **Consumer role:** reacts to `StopFailure{matcher: rate_limit}` from Claude Code, verifies via `claude -p "/usage"`, and routes fallback requests through the Token-Bay network.
- **Seeder role:** accepts forwarded requests during the user's configured idle window and serves them via `claude -p "<prompt>"` with tool-disabling flags.

Authoritative spec: `docs/superpowers/specs/plugin/2026-04-22-plugin-design.md`.

## Non-negotiable rules (plugin-specific)

1. **The plugin never holds an Anthropic API key.** All Anthropic-bound traffic is mediated by the `claude` CLI (the "bridge"). If you find yourself adding a field to store a token, stop and re-read the spec.
2. **Seeder-role `claude -p` invocations disable every side-effecting primitive.** The minimum flag set: `--disallowedTools "*" --mcp-config /dev/null` and empty hooks. Defined in `internal/ccbridge`. Any change to those flags must ship with an updated bridge conformance test.
3. **Never hold or inspect Anthropic auth credentials.** The `internal/ccproxy` local HTTP server on `127.0.0.1` is the sanctioned endpoint for mid-session redirect via `ANTHROPIC_BASE_URL` in `~/.claude/settings.json` (spec §2.5; `docs/superpowers/plans/2026-04-23-plugin-ccproxy.md`). It MUST forward upstream bytes — including the `Authorization` header — verbatim, never parsing, logging, caching, or mutating credentials. No other interception mechanism is allowed.
4. **Audit log append-only.** Never rewrite or truncate `~/.token-bay/audit.log`. Rotation is by file.
5. **Consumer and seeder roles share one binary.** The same `token-bay-sidecar` process handles both; config switches them on. Don't split into two binaries without a very good reason.

## Tech stack

- Go 1.23+
- QUIC via `quic-go`
- CLI via `spf13/cobra`, config via `spf13/viper` + `gopkg.in/yaml.v3`
- Logging via `rs/zerolog`
- Tests via stdlib `testing` + `stretchr/testify`
- Imports `shared/` for wire-format types and Ed25519 helpers

## Project layout

- `cmd/token-bay-sidecar/` — thin entry point; wires subcommands via cobra
- `internal/<module>/` — one directory per subsystem module; unit tests adjacent as `*_test.go`
- `test/e2e/` — end-to-end with stubbed tracker + stubbed `claude`
- `test/conformance/` — bridge conformance: real `claude` binary, adversarial prompts, asserts zero side effects
- `commands/` + `hooks/` + `.claude-plugin/plugin.json` — Claude Code plugin surfaces

## Commands (run from `plugin/`)

| Command | Effect |
|---|---|
| `make test` | Unit + integration tests with race detector |
| `make lint` | `golangci-lint run ./...` |
| `make build` | Build `bin/token-bay-sidecar` |
| `make conformance` | Bridge conformance suite against the installed `claude` binary |
| `make check` | `test` + `lint` |
| `make install` | Build + install as a local Claude Code plugin |

## Bridge conformance — why it's special

The seeder role's entire safety argument rests on `claude -p` tool-disabling flags being airtight. Any change to `internal/ccbridge/` or `test/conformance/` triggers a pre-commit conformance run (enforced via lefthook).

The suite in `test/conformance/` runs a corpus of adversarial prompts (attempts to `Bash`, `Read`, `Write`, `WebFetch`, trigger MCP servers, fire hooks) against a real `claude -p` invocation with the current flag set. It asserts zero observable side effects on a sandboxed filesystem, process table, and network. A regression in Claude Code's flag enforcement is a CVE-class event and blocks seeder advertise until fixed.

The sidecar runs a subset of the conformance suite at startup (`internal/ccbridge/conformance.go`) — if it fails, `advertise=true` is refused.

## Things that look surprising and aren't bugs

- `/token-bay fallback` runs `claude /usage` under a PTY (not `-p`) as part of its gate — `/usage` is a `local-jsx` TUI that bails to a stub when stdout isn't a TTY (spec §5.2). That subprocess is a real API call and could itself hit a rate limit — that's the "degraded proof" path, documented in the exhaustion-proof spec.
- The plugin config has no `anthropic_token` field. Intentional (see rule 1).
- `claude -p` appears to spawn a full Claude Code instance. Intentional; the tool-disabling flags are why that's safe.

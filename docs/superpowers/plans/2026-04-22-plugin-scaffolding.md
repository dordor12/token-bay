# Plugin Scaffolding Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Scaffold the `plugin/` Go module inside the monorepo — Claude Code plugin surfaces, sidecar entry point, `internal/` module directories, plugin-specific CLAUDE.md, Makefile, conformance workflow, and a working smoke test. **Scaffolding only; no feature code.**

**Architecture:** Go sidecar binary (single static build) packaged as a Claude Code plugin. User-facing surfaces (slash commands in markdown, hooks in JSON, plugin manifest) live alongside the Go source in `plugin/`. The sidecar is invoked by hooks and slash commands via subprocess.

**Tech Stack:**
- Go 1.23+
- `quic-go` for QUIC tunnels
- `spf13/cobra` (CLI)
- `spf13/viper` + `gopkg.in/yaml.v3` (config)
- `rs/zerolog` (logging)
- Go stdlib `crypto/ed25519`
- Go stdlib `testing` + `stretchr/testify` (tests)
- Consumes `shared/` for wire format types

**Dependency order:** Runs after **monorepo foundation plan** and **shared library scaffolding plan**. Plugin `go.mod` will `require` the shared module.

---

## Table of contents

1. [Directory design (plugin scope)](#1-directory-design-plugin-scope)
2. [plugin/CLAUDE.md](#2-pluginclaudemd)
3. [TDD approach (component scope)](#3-tdd-approach-component-scope)
4. [Verification (component scope)](#4-verification-component-scope)
5. [Scaffolding tasks](#5-scaffolding-tasks)

---

## 1. Directory design (plugin scope)

```
plugin/
├── .claude-plugin/
│   └── plugin.json
├── commands/                               # slash commands — markdown
│   ├── token-bay-enroll.md
│   ├── token-bay-fallback.md
│   ├── token-bay-status.md
│   ├── token-bay-balance.md
│   ├── token-bay-seed.md
│   └── token-bay-logs.md
├── hooks/                                  # hook JSON
│   ├── stop-failure-rate-limit.json
│   ├── session-start.json
│   └── session-end.json
├── cmd/
│   └── token-bay-sidecar/
│       ├── main.go
│       └── main_test.go
├── internal/
│   ├── sidecar/                            # top-level coordinator
│   ├── hooks/                              # hook-invocation handlers
│   ├── trackerclient/                      # long-lived tracker connection
│   ├── identity/                           # Ed25519 keypair + enrollment
│   ├── ccbridge/                           # claude -p invocation + /usage probe
│   ├── ratelimit/                          # StopFailure + /usage parsing
│   ├── exhaustionproofbuilder/             # v1 two-signal bundle assembly (consumes shared/exhaustionproof)
│   ├── tunnel/                             # QUIC tunnel (consumer + seeder sides)
│   ├── auditlog/                           # append-only local log
│   ├── config/                             # YAML config loading + validation
│   └── envelopebuilder/                    # broker_request envelope construction
├── test/
│   ├── fixtures/                           # canned StopFailure payloads, /usage outputs, adversarial prompts
│   ├── e2e/                                # end-to-end (tracker stub + ccbridge stub)
│   └── conformance/                        # bridge conformance — adversarial prompts
├── CLAUDE.md
├── Makefile
├── go.mod
└── go.sum
```

### 1.1 Responsibility table

| Path | Responsibility | Depends on |
|---|---|---|
| `plugin/internal/sidecar` | Process lifecycle; ties everything together. | all other `internal/` packages |
| `plugin/internal/hooks` | Claude Code hook invocation handlers (translates JSON payloads into actions). | `ratelimit`, `sidecar`, `ccbridge` |
| `plugin/internal/trackerclient` | QUIC connection to tracker; heartbeat; RPCs. | `identity`, `envelopebuilder`, `shared/proto` |
| `plugin/internal/identity` | Ed25519 keypair; Claude Code account binding; enrollment flow. | `ccbridge`, `config`, `shared/signing`, `shared/ids` |
| `plugin/internal/ccbridge` | Invokes `claude -p` (bridge for seeder; `/usage` probe for consumer). Parses output. | — (leaf) |
| `plugin/internal/ratelimit` | `StopFailure` payload parsing; `/usage` output parsing; verdict matrix. | `ccbridge` |
| `plugin/internal/exhaustionproofbuilder` | Builds v1 two-signal bundle; signs. | `identity`, `ratelimit`, `shared/exhaustionproof` |
| `plugin/internal/envelopebuilder` | Broker-request envelope serialization + hashing. | `shared/proto`, `shared/signing`, `exhaustionproofbuilder` |
| `plugin/internal/tunnel` | QUIC consumer↔seeder tunnel. | `trackerclient` for rendezvous |
| `plugin/internal/auditlog` | Append-only local log (plugin spec §8). | `config` |
| `plugin/internal/config` | YAML parsing + validation. | — (leaf) |

---

## 2. plugin/CLAUDE.md

File at `plugin/CLAUDE.md`. Read by Claude Code when working inside the plugin component. The repo-root CLAUDE.md is also in effect.

````markdown
# plugin — Development Context

## What this is

The Token-Bay Claude Code plugin. Runs on end-user machines. Two roles:

- **Consumer role:** reacts to `StopFailure{matcher: rate_limit}` from Claude Code, verifies via `claude -p "/usage"`, and routes fallback requests through the Token-Bay network.
- **Seeder role:** accepts forwarded requests during the user's configured idle window and serves them via `claude -p "<prompt>"` with tool-disabling flags.

Authoritative spec: `docs/superpowers/specs/plugin/2026-04-22-plugin-design.md`.

## Non-negotiable rules (plugin-specific)

1. **The plugin never holds an Anthropic API key.** All Anthropic-bound traffic is mediated by the `claude` CLI (the "bridge"). If you find yourself adding a field to store a token, stop and re-read the spec.
2. **Seeder-role `claude -p` invocations disable every side-effecting primitive.** The minimum flag set: `--disallowedTools "*" --mcp-config /dev/null` and empty hooks. Defined in `internal/ccbridge`. Any change to those flags must ship with an updated bridge conformance test.
3. **Never intercept Claude Code's HTTPS traffic.** The plugin is hook-driven and slash-command-driven. No local proxy. No modifying Claude Code's API base URL.
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

- `/token-bay fallback` runs `claude -p "/usage"` as part of its gate. That subprocess is a real API call and could itself hit a rate limit — that's the "degraded proof" path, documented in the exhaustion-proof spec.
- The plugin config has no `anthropic_token` field. Intentional (see rule 1).
- `claude -p` appears to spawn a full Claude Code instance. Intentional; the tool-disabling flags are why that's safe.
````

---

## 3. TDD approach (component scope)

The repo-level CLAUDE.md already states TDD is non-negotiable. Plugin-specific additions:

- **Unit tests live adjacent to code** as `*_test.go`, inside the same package.
- **Integration tests** live in `plugin/test/e2e/`. They use a local tracker stub and a local `claude` bridge stub (fixture-driven) so the full consumer/seeder flow can be exercised without real network or real `claude`.
- **Conformance tests** live in `plugin/test/conformance/`. Separate from the regular test run (build tag `conformance`), because they are slow and require a real `claude` binary.
- **Fixtures** (canned `StopFailure` payloads, `/usage` outputs, adversarial prompts) live in `plugin/test/fixtures/` and are shared between unit tests and integration tests.
- **Mocking policy:** prefer fixture-driven integration over per-method mocks. The `ccbridge` has an injectable `BridgeRunner` interface whose default implementation `exec.Command`s `claude`; tests swap in a stub that reads fixture files. This pattern is applied consistently across modules that call out to external processes.
- **Race detector on every run.** `go test -race` is always on — concurrency bugs in a networked sidecar are unacceptable.
- **Coverage target:** 80% statements across `internal/` overall, with behavior-rich modules (`ratelimit`, `exhaustionproofbuilder`, `ccbridge`) prioritized.

---

## 4. Verification (component scope)

The plugin inherits the repo-level lint and CI workflows from the foundation plan. Plugin-specific additions:

- **Bridge conformance CI workflow** at `.github/workflows/plugin-conformance.yml`. Triggers: PR that touches `plugin/internal/ccbridge/**` or `plugin/test/conformance/**`, plus a nightly cron to catch upstream Claude Code changes.
- **Startup conformance check** in `internal/ccbridge/conformance.go` — a subset of the full suite runs on sidecar launch; seeder role is refused if it fails.
- **E2E smoke** in `plugin/test/e2e/` — boots tracker stub + seeder sidecar + consumer sidecar, simulates a `StopFailure`, asserts ledger preimage exchange round-trips. Must run in under 30 seconds.
- **Install verification** — `make install` script runs a post-install check that confirms the plugin was registered with the local Claude Code instance.

---

## 5. Scaffolding tasks

### Task 1: Initialize the plugin module

**Files:**
- Create: `/Users/dor.amid/git/token-bay/plugin/go.mod`
- Remove: `/Users/dor.amid/git/token-bay/plugin/.gitkeep`

- [ ] **Step 1: Remove placeholder**

Run:
```bash
cd /Users/dor.amid/git/token-bay
rm plugin/.gitkeep
```

- [ ] **Step 2: Initialize Go module**

Run:
```bash
cd /Users/dor.amid/git/token-bay/plugin
go mod init github.com/YOUR_ORG/token-bay/plugin
```

- [ ] **Step 3: Add plugin-specific dependencies**

Run:
```bash
go get github.com/spf13/cobra@latest
go get github.com/spf13/viper@latest
go get github.com/rs/zerolog@latest
go get github.com/stretchr/testify@latest
go get github.com/quic-go/quic-go@latest
go get gopkg.in/yaml.v3@latest
go get golang.org/x/sync/errgroup@latest
go mod tidy
```

- [ ] **Step 4: Wire shared module as a local require**

Edit `plugin/go.mod` — add the `require` and matching `replace` (workspace will resolve the replace path during local development):
```go
require github.com/YOUR_ORG/token-bay/shared v0.0.0-00010101000000-000000000000

replace github.com/YOUR_ORG/token-bay/shared => ../shared
```

Run: `go mod tidy`

- [ ] **Step 5: Register in `go.work`**

Edit `/Users/dor.amid/git/token-bay/go.work` — append:
```
use ./plugin
```

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add plugin/go.mod plugin/go.sum go.work
git rm plugin/.gitkeep
git commit -m "chore(plugin): initialize Go module"
```

### Task 2: Scaffold the plugin directory tree

**Files:**
- Create: empty directories per §1 layout, with `.gitkeep` where needed.

- [ ] **Step 1: Create directory tree**

Run:
```bash
cd /Users/dor.amid/git/token-bay/plugin
mkdir -p .claude-plugin commands hooks
mkdir -p cmd/token-bay-sidecar
mkdir -p internal/{sidecar,hooks,trackerclient,identity,ccbridge,ratelimit,exhaustionproofbuilder,envelopebuilder,tunnel,auditlog,config}
mkdir -p test/{fixtures,e2e,conformance}
```

- [ ] **Step 2: `.gitkeep` empty dirs**

Run:
```bash
cd /Users/dor.amid/git/token-bay/plugin
find .claude-plugin commands hooks cmd internal test -type d -empty -exec touch {}/.gitkeep \;
```

- [ ] **Step 3: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add plugin/
git commit -m "chore(plugin): scaffold directory tree"
```

### Task 3: Write `plugin/CLAUDE.md`

**Files:**
- Create: `/Users/dor.amid/git/token-bay/plugin/CLAUDE.md`

- [ ] **Step 1: Write the CLAUDE.md**

Copy the content from §2 of this plan verbatim into `/Users/dor.amid/git/token-bay/plugin/CLAUDE.md`.

- [ ] **Step 2: Commit**

```bash
git add plugin/CLAUDE.md
git commit -m "docs(plugin): add component CLAUDE.md"
```

### Task 4: Add plugin Makefile

**Files:**
- Create: `/Users/dor.amid/git/token-bay/plugin/Makefile`

- [ ] **Step 1: Write Makefile**

Write to `/Users/dor.amid/git/token-bay/plugin/Makefile`:
```makefile
.PHONY: all test lint check build conformance clean install

all: check build

build:
	go build -o bin/token-bay-sidecar ./cmd/token-bay-sidecar

test:
	go test -race -coverprofile=coverage.out ./...

lint:
	golangci-lint run ./...

conformance:
	@echo "Running bridge conformance suite..."
	go test -tags=conformance ./test/conformance/...

check: test lint

clean:
	rm -rf bin/ coverage.out

install: build
	@echo "TODO: install as Claude Code plugin (pending installer scripting in feature plan)"
```

- [ ] **Step 2: Commit**

```bash
git add plugin/Makefile
git commit -m "chore(plugin): add Makefile"
```

### Task 5: Minimal sidecar entry point with a passing test

**Files:**
- Create: `/Users/dor.amid/git/token-bay/plugin/cmd/token-bay-sidecar/main.go`
- Create: `/Users/dor.amid/git/token-bay/plugin/cmd/token-bay-sidecar/main_test.go`
- Remove: `plugin/cmd/token-bay-sidecar/.gitkeep`

- [ ] **Step 1: Write the failing test**

Write to `/Users/dor.amid/git/token-bay/plugin/cmd/token-bay-sidecar/main_test.go`:
```go
package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRootCmd_Version_PrintsExpected(t *testing.T) {
	var buf bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"version"})

	err := cmd.Execute()

	assert.NoError(t, err)
	assert.Contains(t, buf.String(), "token-bay-sidecar")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run (from `plugin/`):
```bash
go test ./cmd/token-bay-sidecar/...
```
Expected: FAIL — `undefined: newRootCmd`.

- [ ] **Step 3: Write minimal `main.go`**

Write to `/Users/dor.amid/git/token-bay/plugin/cmd/token-bay-sidecar/main.go`:
```go
// Package main is the token-bay-sidecar entry point.
//
// The sidecar is the long-lived process behind the Token-Bay Claude Code
// plugin. It coordinates the tracker connection, consumer-side fallback
// pipeline, and seeder-side bridge invocation.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

const version = "0.0.0-dev"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "token-bay-sidecar",
		Short: "Token-Bay plugin sidecar",
	}
	root.AddCommand(newVersionCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the sidecar version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "token-bay-sidecar %s\n", version)
		},
	}
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
```

- [ ] **Step 4: Remove the .gitkeep**

Run:
```bash
rm /Users/dor.amid/git/token-bay/plugin/cmd/token-bay-sidecar/.gitkeep
```

- [ ] **Step 5: Run test to verify it passes**

Run (from `plugin/`):
```bash
go test ./cmd/token-bay-sidecar/...
```
Expected: PASS.

- [ ] **Step 6: Build and smoke-test**

Run (from `plugin/`):
```bash
go build -o bin/token-bay-sidecar ./cmd/token-bay-sidecar
./bin/token-bay-sidecar version
```
Expected output: `token-bay-sidecar 0.0.0-dev`.

- [ ] **Step 7: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add plugin/cmd/token-bay-sidecar/
git commit -m "feat(plugin): minimal sidecar entry point with version command"
```

### Task 6: Minimal Claude Code plugin manifest

**Files:**
- Create: `/Users/dor.amid/git/token-bay/plugin/.claude-plugin/plugin.json`
- Remove: `plugin/.claude-plugin/.gitkeep`

- [ ] **Step 1: Write manifest**

Write to `/Users/dor.amid/git/token-bay/plugin/.claude-plugin/plugin.json`:
```json
{
  "name": "token-bay",
  "version": "0.0.0-dev",
  "description": "Claude Code plugin for the Token-Bay educational distributed rate-limit sharing network.",
  "commands": [],
  "hooks": []
}
```

(Slash commands and hook entries are populated in subsequent feature plans as each surface is implemented.)

- [ ] **Step 2: Remove .gitkeep**

Run:
```bash
rm /Users/dor.amid/git/token-bay/plugin/.claude-plugin/.gitkeep
```

- [ ] **Step 3: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add plugin/.claude-plugin/
git commit -m "feat(plugin): add minimal Claude Code plugin manifest"
```

### Task 7: Scaffold bridge conformance harness (placeholder)

**Files:**
- Create: `/Users/dor.amid/git/token-bay/plugin/test/conformance/harness_test.go`
- Remove: `plugin/test/conformance/.gitkeep`

- [ ] **Step 1: Write placeholder conformance test**

Write to `/Users/dor.amid/git/token-bay/plugin/test/conformance/harness_test.go`:
```go
//go:build conformance
// +build conformance

// Package conformance runs adversarial prompts against a real `claude -p`
// invocation with the current tool-disabling flag set and asserts zero
// observable side effects. This test is the load-bearing check on the
// seeder role's safety argument.
//
// Implemented progressively in the feature plan for ccbridge.
package conformance

import "testing"

func TestBridgeConformance_Placeholder(t *testing.T) {
	t.Skip("bridge conformance corpus not yet implemented — tracked by internal/ccbridge feature plan")
}
```

- [ ] **Step 2: Remove .gitkeep**

```bash
rm /Users/dor.amid/git/token-bay/plugin/test/conformance/.gitkeep
```

- [ ] **Step 3: Confirm it runs**

Run (from `plugin/`):
```bash
go test -tags=conformance ./test/conformance/...
```
Expected: a single skipped test.

- [ ] **Step 4: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add plugin/test/conformance/
git commit -m "test(plugin): scaffold bridge conformance harness"
```

### Task 8: Plugin-specific conformance CI workflow

**Files:**
- Create: `/Users/dor.amid/git/token-bay/.github/workflows/plugin-conformance.yml`

- [ ] **Step 1: Write workflow**

Write to `/Users/dor.amid/git/token-bay/.github/workflows/plugin-conformance.yml`:
```yaml
name: Plugin Bridge Conformance

on:
  pull_request:
    paths:
      - 'plugin/internal/ccbridge/**'
      - 'plugin/test/conformance/**'
      - 'plugin/test/fixtures/**'
  schedule:
    - cron: '0 3 * * *'  # 03:00 UTC nightly
  workflow_dispatch:

jobs:
  conformance:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.23'
      - name: Sync workspace
        run: go work sync
      # TODO (later feature plan): install the `claude` CLI here so the
      # real conformance suite can execute. Until then, the placeholder
      # test runs and skips.
      - name: Run conformance
        run: make -C plugin conformance
```

- [ ] **Step 2: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add .github/workflows/plugin-conformance.yml
git commit -m "ci(plugin): add bridge conformance workflow"
```

### Task 9: Extend root lefthook with plugin conformance pre-commit

**Files:**
- Modify: `/Users/dor.amid/git/token-bay/lefthook.yml`

- [ ] **Step 1: Append conformance hook**

Edit `/Users/dor.amid/git/token-bay/lefthook.yml` — append:
```yaml
    plugin-conformance:
      glob:
        - 'plugin/internal/ccbridge/**/*.go'
        - 'plugin/test/conformance/**/*.go'
        - 'plugin/test/fixtures/**'
      run: make -C plugin conformance
```

(This runs the conformance suite only when the listed paths are in the staged set. Until the feature plan implements the suite, it will be a skipped test — cheap.)

- [ ] **Step 2: Commit**

```bash
git add lefthook.yml
git commit -m "chore(plugin): add conformance pre-commit hook"
```

### Task 10: Verify full check

**Files:**
- None (verification only)

- [ ] **Step 1: Run plugin tests**

Run: `make -C plugin test`
Expected: version test passes.

- [ ] **Step 2: Run plugin conformance**

Run: `make -C plugin conformance`
Expected: single skipped test.

- [ ] **Step 3: Run plugin build**

Run: `make -C plugin build && ls -la plugin/bin/token-bay-sidecar`
Expected: binary exists and runs `./plugin/bin/token-bay-sidecar version`.

- [ ] **Step 4: Run root `make check`**

Run: `make check` (from repo root)
Expected: delegates to shared and plugin; both green.

- [ ] **Step 5: Tag**

```bash
git tag -a plugin-v0 -m "Plugin scaffolding complete"
```

---

## Self-review

- **Spec coverage:** The spec's five user-requested areas (tech stack, directory, CLAUDE.md, TDD, verification) are each addressed in this plan. No feature implementation — deferred to per-module feature plans.
- **Placeholder scan:** Two `TODO`s remain, both intentional: the `make install` target (pending installer-scripting feature plan) and the conformance CI's claude-CLI install step (pending Claude Code CI setup). Both are documented as "later feature plan".
- **Type consistency:** `newRootCmd` / `newVersionCmd` consistent with Task 5.
- **Dependencies:** Plugin module requires `shared/` (declared in Task 1 step 4). Workspace stitches them together locally.

## Next plan

After this plan, proceed to the **tracker scaffolding plan** (`2026-04-22-tracker-scaffolding.md`).

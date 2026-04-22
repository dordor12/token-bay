# Tracker Scaffolding Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Scaffold `tracker/` as a Go module inside the monorepo — server entry point, `internal/` modules for broker, ledger, federation, reputation, registry, STUN/TURN, API surface, and config; tracker-specific CLAUDE.md, Makefile, and a working smoke test. **Scaffolding only; no feature code.**

**Architecture:** Long-running Go server. One process per regional tracker. QUIC-fronted RPC for plugin clients and peer trackers. SQLite (WAL mode) as the default ledger backend for v1. Federation, ledger, and reputation live as internal modules — they are not standalone services.

**Tech Stack:**
- Go 1.23+
- `quic-go` for QUIC listener
- SQLite via `modernc.org/sqlite` (pure-Go driver; no cgo; easy cross-compile)
- `spf13/cobra` (CLI), `spf13/viper` + `gopkg.in/yaml.v3` (config)
- `rs/zerolog` (logging)
- `prometheus/client_golang` (metrics)
- Go stdlib `crypto/ed25519`
- Go stdlib `testing` + `stretchr/testify` (tests)
- Consumes `shared/` for wire format types

**Dependency order:** Runs after **monorepo foundation** and **shared library scaffolding**. Tracker `go.mod` will `require` the shared module.

---

## Table of contents

1. [Directory design (tracker scope)](#1-directory-design-tracker-scope)
2. [tracker/CLAUDE.md](#2-trackerclaudemd)
3. [TDD approach (component scope)](#3-tdd-approach-component-scope)
4. [Verification (component scope)](#4-verification-component-scope)
5. [Scaffolding tasks](#5-scaffolding-tasks)

---

## 1. Directory design (tracker scope)

```
tracker/
├── CLAUDE.md
├── Makefile
├── go.mod
├── go.sum
├── cmd/
│   └── token-bay-tracker/
│       ├── main.go
│       └── main_test.go
├── internal/
│   ├── server/                             # server process lifecycle
│   ├── api/                                # QUIC/TLS listener + RPC dispatch
│   ├── session/                            # per-connection state machine
│   ├── registry/                           # live seeder registry
│   ├── broker/                             # request broker + selection
│   ├── ledger/                             # append-only chain + Merkle + balance projection
│   │   ├── entry/                          # entry serialization + signing
│   │   └── storage/                        # SQLite backend (modernc driver)
│   ├── federation/                         # peer connections + gossip
│   ├── reputation/                         # signal collection + scoring + OK/AUDIT/FROZEN
│   ├── stunturn/                           # STUN reflection + TURN relay
│   ├── admin/                              # admin HTTP API
│   ├── config/                             # YAML config loading + validation
│   └── metrics/                            # prometheus counters + histograms
├── test/
│   ├── fixtures/                           # canned ledger states, peer handshakes, etc.
│   ├── e2e/                                # end-to-end tests with plugin stub
│   └── integration/                        # cross-internal-module integration
├── deployments/
│   ├── systemd/                            # systemd unit for long-running deployment
│   └── docker/                             # Dockerfile for tracker image
└── scripts/
    ├── dev-run-local.sh                    # local dev helper
    └── seed-bootstrap-keys.sh              # local helper to generate tracker keypair
```

### 1.1 Responsibility table

| Path | Responsibility | Depends on |
|---|---|---|
| `tracker/internal/server` | Process lifecycle; wires all other modules; graceful shutdown. | all other `internal/` |
| `tracker/internal/api` | QUIC/TLS listener; RPC demux; envelope validation entry point. | `session`, `shared/proto` |
| `tracker/internal/session` | Per-connection state machine for plugin and peer connections. | `registry`, `broker`, `federation` |
| `tracker/internal/registry` | Live seeder registry (sharded in-memory). | `shared/ids` |
| `tracker/internal/broker` | Selection function + offer orchestration + settlement. | `registry`, `ledger`, `reputation`, `shared/proto`, `shared/exhaustionproof` |
| `tracker/internal/ledger` | Append-only signed log + Merkle roots + balance projection. | `storage`, `entry`, `shared/signing` |
| `tracker/internal/ledger/entry` | Entry struct + canonical bytes + signing/verification. | `shared/proto`, `shared/signing` |
| `tracker/internal/ledger/storage` | SQLite persistence for entries, balances, roots. | `modernc.org/sqlite` |
| `tracker/internal/federation` | Peer handshake, gossip, transfer proofs, revocation. | `ledger`, `shared/proto` |
| `tracker/internal/reputation` | Signal collection + z-score + OK/AUDIT/FROZEN state. | `ledger`, `shared/ids` |
| `tracker/internal/stunturn` | STUN reflection + TURN relay fallback for consumer↔seeder hole-punching. | — (leaf) |
| `tracker/internal/admin` | Admin HTTP API (ops, debug, dispute). | `broker`, `reputation`, `ledger` |
| `tracker/internal/config` | YAML config parsing + validation. | — (leaf) |
| `tracker/internal/metrics` | Prometheus counters + histograms. | — (leaf) |

### 1.2 Why the split

Federation, ledger, and reputation are their own subsystems in the spec but they do not warrant separate binaries. They are invoked synchronously from broker settlement paths and federation gossip paths; running them out-of-process would add latency without a corresponding benefit. Having them as internal modules with clear interfaces gives us the testability and modularity of separate services with none of the operational cost.

---

## 2. tracker/CLAUDE.md

File at `tracker/CLAUDE.md`. Repo-root CLAUDE.md is also in effect.

````markdown
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
- `internal/broker` and `internal/federation` are the most concurrent modules. Run `go test -race` by default; flakes there are always real bugs.
- Use the Go workspace: a change in `shared/` that touches a proto struct will affect the tracker's compilation immediately; fix both sides in the same commit.

## Things that look surprising and aren't bugs

- The tracker does not know Anthropic's rate limits. It trusts the seeder's self-reported usage and catches systematic dishonesty via `internal/reputation` (see reputation spec).
- The ledger balance projection is a cache. The chain is the source of truth; the projection is rebuildable by replay.
- SQLite is the v1 backend. Yes, for a "distributed system." Regional trackers are single-process; SQLite is enough. Switching to a bigger DB happens if/when a region outgrows one process.
````

---

## 3. TDD approach (component scope)

Tracker-specific additions on top of the repo-level discipline:

- **Unit tests adjacent** as `*_test.go`. One test file per Go file.
- **Integration tests** live in `tracker/test/integration/` and exercise across internal modules using the real SQLite backend with a temp DB.
- **End-to-end tests** in `tracker/test/e2e/` boot the full server on a random port with a stubbed plugin client and peer-tracker stub. Test budget: 30 seconds each; a slow e2e is usually a sign the test is doing too much.
- **Fixtures** in `tracker/test/fixtures/`:
  - Canned ledger entries (genesis, simple usage, transfer in, transfer out).
  - Canned peer handshakes.
  - Canned broker envelopes (valid + tampered variants for validator testing).
- **SQLite testing pattern.** Every test that hits the DB gets its own tempdir. Use `t.TempDir()`; never a package-global path.
- **Concurrency testing.** `internal/broker` and `internal/federation` use goroutines; every test in those packages runs under `-race`. The test file header comment declares it.

---

## 4. Verification (component scope)

Tracker inherits repo-level lint and CI. Tracker-specific:

- **`go-sqlite3` avoided intentionally.** Using `modernc.org/sqlite` (pure Go) means no cgo, no cross-compile pain, no OS-specific test failures. The lint config warns on any `github.com/mattn/go-sqlite3` import.
- **Server startup integration test** in `test/e2e/server_boot_test.go`: boots the server with a tempdir config, queries the admin `/health` endpoint, shuts down gracefully. Runs in CI.
- **Ledger invariant test.** A periodic CI job replays all entries from fixtures and asserts the balance projection matches the chain-replay balance. Catches any divergence bug in `internal/ledger/storage`.
- **Federation two-tracker test.** `test/e2e/federation_peers_test.go` boots two trackers, peers them, performs a Merkle-root exchange, asserts each tracker archives the other's root correctly.
- **`make run-local`** script generates a throwaway keypair and SQLite DB under `.token-bay-local/` (gitignored) so developers can spin up a tracker in seconds. Intended for manual testing against a plugin running locally.

---

## 5. Scaffolding tasks

### Task 1: Initialize the tracker module

**Files:**
- Create: `/Users/dor.amid/git/token-bay/tracker/go.mod`
- Remove: `/Users/dor.amid/git/token-bay/tracker/.gitkeep`

- [ ] **Step 1: Remove placeholder**

Run:
```bash
cd /Users/dor.amid/git/token-bay
rm tracker/.gitkeep
```

- [ ] **Step 2: Initialize Go module**

Run:
```bash
cd /Users/dor.amid/git/token-bay/tracker
go mod init github.com/YOUR_ORG/token-bay/tracker
```

- [ ] **Step 3: Add tracker-specific dependencies**

Run:
```bash
go get github.com/spf13/cobra@latest
go get github.com/spf13/viper@latest
go get github.com/rs/zerolog@latest
go get github.com/stretchr/testify@latest
go get github.com/quic-go/quic-go@latest
go get modernc.org/sqlite@latest
go get github.com/prometheus/client_golang@latest
go get gopkg.in/yaml.v3@latest
go get golang.org/x/sync/errgroup@latest
go mod tidy
```

- [ ] **Step 4: Wire shared module**

Edit `tracker/go.mod` — add:
```go
require github.com/YOUR_ORG/token-bay/shared v0.0.0-00010101000000-000000000000

replace github.com/YOUR_ORG/token-bay/shared => ../shared
```

Run: `go mod tidy`

- [ ] **Step 5: Register in `go.work`**

Edit `/Users/dor.amid/git/token-bay/go.work` — append:
```
use ./tracker
```

- [ ] **Step 6: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add tracker/go.mod tracker/go.sum go.work
git rm tracker/.gitkeep
git commit -m "chore(tracker): initialize Go module"
```

### Task 2: Scaffold the tracker directory tree

**Files:**
- Create: empty directories per §1, with `.gitkeep` where needed.

- [ ] **Step 1: Create directory tree**

Run:
```bash
cd /Users/dor.amid/git/token-bay/tracker
mkdir -p cmd/token-bay-tracker
mkdir -p internal/server
mkdir -p internal/api
mkdir -p internal/session
mkdir -p internal/registry
mkdir -p internal/broker
mkdir -p internal/ledger/{entry,storage}
mkdir -p internal/federation
mkdir -p internal/reputation
mkdir -p internal/stunturn
mkdir -p internal/admin
mkdir -p internal/config
mkdir -p internal/metrics
mkdir -p test/{fixtures,e2e,integration}
mkdir -p deployments/{systemd,docker}
mkdir -p scripts
```

- [ ] **Step 2: `.gitkeep` empty dirs**

Run:
```bash
cd /Users/dor.amid/git/token-bay/tracker
find cmd internal test deployments scripts -type d -empty -exec touch {}/.gitkeep \;
```

- [ ] **Step 3: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add tracker/
git commit -m "chore(tracker): scaffold directory tree"
```

### Task 3: Write `tracker/CLAUDE.md`

**Files:**
- Create: `/Users/dor.amid/git/token-bay/tracker/CLAUDE.md`

- [ ] **Step 1: Write CLAUDE.md**

Copy the content from §2 of this plan verbatim into `/Users/dor.amid/git/token-bay/tracker/CLAUDE.md`.

- [ ] **Step 2: Commit**

```bash
git add tracker/CLAUDE.md
git commit -m "docs(tracker): add component CLAUDE.md"
```

### Task 4: Add tracker Makefile

**Files:**
- Create: `/Users/dor.amid/git/token-bay/tracker/Makefile`

- [ ] **Step 1: Write Makefile**

Write to `/Users/dor.amid/git/token-bay/tracker/Makefile`:
```makefile
.PHONY: all test lint check build clean run-local docker

all: check build

build:
	go build -o bin/token-bay-tracker ./cmd/token-bay-tracker

test:
	go test -race -coverprofile=coverage.out ./...

lint:
	golangci-lint run ./...

check: test lint

clean:
	rm -rf bin/ coverage.out .token-bay-local/

run-local: build
	./scripts/dev-run-local.sh

docker:
	docker build -f deployments/docker/Dockerfile -t token-bay-tracker:dev ../
```

- [ ] **Step 2: Commit**

```bash
git add tracker/Makefile
git commit -m "chore(tracker): add Makefile"
```

### Task 5: Minimal tracker entry point with passing test

**Files:**
- Create: `/Users/dor.amid/git/token-bay/tracker/cmd/token-bay-tracker/main.go`
- Create: `/Users/dor.amid/git/token-bay/tracker/cmd/token-bay-tracker/main_test.go`
- Remove: `tracker/cmd/token-bay-tracker/.gitkeep`

- [ ] **Step 1: Write the failing test**

Write to `/Users/dor.amid/git/token-bay/tracker/cmd/token-bay-tracker/main_test.go`:
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
	assert.Contains(t, buf.String(), "token-bay-tracker")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run (from `tracker/`):
```bash
go test ./cmd/token-bay-tracker/...
```
Expected: FAIL — `undefined: newRootCmd`.

- [ ] **Step 3: Write minimal `main.go`**

Write to `/Users/dor.amid/git/token-bay/tracker/cmd/token-bay-tracker/main.go`:
```go
// Package main is the token-bay-tracker entry point.
//
// The tracker is the regional coordination server: seeder registry, request
// broker, credit ledger owner, STUN/TURN rendezvous, federation peer.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

const version = "0.0.0-dev"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "token-bay-tracker",
		Short: "Token-Bay regional tracker server",
	}
	root.AddCommand(newVersionCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the tracker version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "token-bay-tracker %s\n", version)
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

- [ ] **Step 4: Remove .gitkeep**

```bash
rm /Users/dor.amid/git/token-bay/tracker/cmd/token-bay-tracker/.gitkeep
```

- [ ] **Step 5: Run test to verify it passes**

Run (from `tracker/`):
```bash
go test ./cmd/token-bay-tracker/...
```
Expected: PASS.

- [ ] **Step 6: Build and smoke-test**

Run (from `tracker/`):
```bash
go build -o bin/token-bay-tracker ./cmd/token-bay-tracker
./bin/token-bay-tracker version
```
Expected output: `token-bay-tracker 0.0.0-dev`.

- [ ] **Step 7: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add tracker/cmd/token-bay-tracker/
git commit -m "feat(tracker): minimal server entry point with version command"
```

### Task 6: Local dev runner script

**Files:**
- Create: `/Users/dor.amid/git/token-bay/tracker/scripts/dev-run-local.sh`
- Remove: `tracker/scripts/.gitkeep`

- [ ] **Step 1: Write script**

Write to `/Users/dor.amid/git/token-bay/tracker/scripts/dev-run-local.sh`:
```bash
#!/usr/bin/env bash
set -euo pipefail

# Local development runner for token-bay-tracker.
#
# Boots the tracker with a throwaway keypair and SQLite DB under
# ./.token-bay-local/. Intended for manual testing against a locally-running
# plugin. The directory is gitignored.

HERE="$(cd "$(dirname "$0")/.." && pwd)"
DATA_DIR="${HERE}/.token-bay-local"
mkdir -p "${DATA_DIR}"

echo "=== token-bay-tracker local dev run ==="
echo "  data dir: ${DATA_DIR}"
echo "  binary:   ${HERE}/bin/token-bay-tracker"
echo ""
echo "TODO: wire this script to the server 'run' subcommand once it lands"
echo "in a subsequent feature plan. For now this script is a placeholder so"
echo "the Makefile 'run-local' target has something to invoke."
```

- [ ] **Step 2: Make executable**

Run:
```bash
chmod +x /Users/dor.amid/git/token-bay/tracker/scripts/dev-run-local.sh
```

- [ ] **Step 3: Remove .gitkeep**

```bash
rm /Users/dor.amid/git/token-bay/tracker/scripts/.gitkeep
```

- [ ] **Step 4: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add tracker/scripts/
git commit -m "chore(tracker): add dev-run-local placeholder script"
```

### Task 7: Docker + systemd placeholders

**Files:**
- Create: `/Users/dor.amid/git/token-bay/tracker/deployments/docker/Dockerfile`
- Create: `/Users/dor.amid/git/token-bay/tracker/deployments/systemd/token-bay-tracker.service`
- Remove: `tracker/deployments/docker/.gitkeep`, `tracker/deployments/systemd/.gitkeep`

- [ ] **Step 1: Write minimal Dockerfile**

Write to `/Users/dor.amid/git/token-bay/tracker/deployments/docker/Dockerfile`:
```dockerfile
# syntax=docker/dockerfile:1.6

FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY . .
RUN go work sync \
 && cd tracker \
 && CGO_ENABLED=0 go build -o /out/token-bay-tracker ./cmd/token-bay-tracker

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=builder /out/token-bay-tracker /usr/local/bin/token-bay-tracker
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/token-bay-tracker"]
CMD ["--help"]
```

- [ ] **Step 2: Write systemd unit**

Write to `/Users/dor.amid/git/token-bay/tracker/deployments/systemd/token-bay-tracker.service`:
```ini
[Unit]
Description=Token-Bay regional tracker
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=token-bay
Group=token-bay
ExecStart=/usr/local/bin/token-bay-tracker run --config /etc/token-bay/tracker.yaml
Restart=on-failure
RestartSec=5s
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/token-bay

[Install]
WantedBy=multi-user.target
```

- [ ] **Step 3: Remove .gitkeeps**

```bash
rm /Users/dor.amid/git/token-bay/tracker/deployments/docker/.gitkeep
rm /Users/dor.amid/git/token-bay/tracker/deployments/systemd/.gitkeep
```

- [ ] **Step 4: Commit**

```bash
cd /Users/dor.amid/git/token-bay
git add tracker/deployments/
git commit -m "chore(tracker): add Docker + systemd deployment placeholders"
```

### Task 8: Verify full check

**Files:**
- None (verification only)

- [ ] **Step 1: Run tracker tests**

Run: `make -C tracker test`
Expected: version test passes.

- [ ] **Step 2: Run tracker build**

Run: `make -C tracker build && ls -la tracker/bin/token-bay-tracker`
Expected: binary exists and runs `./tracker/bin/token-bay-tracker version`.

- [ ] **Step 3: Run root `make check`**

Run: `make check` (from repo root)
Expected: delegates to shared, plugin, tracker — all three green.

- [ ] **Step 4: Tag**

```bash
git tag -a tracker-v0 -m "Tracker scaffolding complete"
```

---

## Self-review

- **Spec coverage:** Same five areas — tech stack, directory design, CLAUDE.md, TDD approach, verification — all present. No feature code; that's what the subsequent feature plans for each `internal/` module deliver.
- **Placeholder scan:** Two intentional `TODO`s — the `dev-run-local.sh` script body (placeholder until the server run subcommand is implemented in a feature plan) and the install-time step count on docker. Both documented in place.
- **Type consistency:** `newRootCmd` / `newVersionCmd` consistent within Task 5. Entry-point pattern parallels the plugin's entry point for consistency.
- **Dependencies:** tracker `go.mod` requires `shared`; confirmed in Task 1 step 4. Workspace stitches locally.

## Next plans (feature-level, after scaffolding)

Each of the internal modules gets its own feature-implementation plan. Natural ordering, by dependency:

1. `internal/config` — YAML loading.
2. `internal/ledger/entry` — entry canonical bytes + signing.
3. `internal/ledger/storage` — SQLite backend.
4. `internal/ledger` — chain + balance projection + Merkle roots (ties entry + storage).
5. `internal/registry` — seeder registry.
6. `internal/reputation` — signals + state machine.
7. `internal/broker` — selection + settlement.
8. `internal/federation` — peer handshake + gossip + transfer proofs.
9. `internal/stunturn` — STUN + TURN.
10. `internal/api` — QUIC listener + RPC dispatch.
11. `internal/admin` — operator HTTP API.
12. `internal/server` — process lifecycle + wiring.

Each feature plan produces a commit-per-task, ships with a full test suite, and stops before the next module starts.

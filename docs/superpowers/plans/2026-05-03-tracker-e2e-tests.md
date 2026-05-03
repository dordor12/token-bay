# Tracker e2e tests — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land a `tracker/test/e2e/` test package that drives the regional tracker through every public boundary that exists today: the compiled `token-bay-tracker` binary's CLI surface, and the in-process composition of the three implemented subsystems (`internal/config`, `internal/ledger`, `internal/registry`) along the same wiring path the eventual `internal/server` will use. Each test asserts a contract that lives at the user-visible perimeter — exit codes, ledger durability across reopens, registry candidate matching against a config-derived shape — and that no single subsystem's unit tests can prove on its own.

**Architecture:** Single new external test package (`package e2e_test`) under `tracker/test/e2e/`. Two surfaces:

1. **Binary surface.** `go build` the tracker into a `t.TempDir()`-rooted binary, then exec it via `os/exec` against fixture YAMLs. Asserts stdout/stderr and the spec-defined exit codes (`config validate` returns 0/1/2/3 per design spec §3.3). This is the only surface today's tracker exposes to an operator.
2. **Composed-state surface.** From inside the test process, `config.Load` a fixture YAML, `storage.Open` over the `ledger.storage_path` it points at, `ledger.Open` over that store with a deterministic tracker keypair, `registry.New` sized from the broker section of the same config. Drive a small operator-visible scenario across the three (issue starter grant for a fake identity → register a fake seeder → match candidates → query SignedBalance → `AssertChainIntegrity`). This is the in-process stand-in for what `internal/server.Run` will eventually do — it pins the boundary contract so when the listener subsystem lands, the e2e tests grow from "compose modules in-process" to "drive modules through the listener" with the same test scenarios.

Tests are fully hermetic — `t.TempDir()` for every filesystem path, deterministic Ed25519 seeds for the tracker key, a fixed clock injected via `ledger.WithClock`. No real plugins, no real federation peers, no QUIC, no admin HTTP. Build tag is unset so they run on every `make -C tracker test`.

**Tech Stack:**
- Go 1.23 stdlib (`os/exec`, `crypto/ed25519`, `crypto/sha256`, `context`, `path/filepath`, `time`)
- `github.com/stretchr/testify/assert` + `require`
- Imports under test: `tracker/cmd/token-bay-tracker` (compiled), `tracker/internal/config`, `tracker/internal/ledger`, `tracker/internal/ledger/storage`, `tracker/internal/registry`
- Imports from `shared/`: `proto`, `ids`

**Spec references:**
- Tracker design: `docs/superpowers/specs/tracker/2026-04-22-tracker-design.md` §3 (internal modules), §4.1 (SeederRecord), §5.1 (broker filter shape — registry provides the candidate set), §11 (acceptance criteria — "Ledger settlements always have valid tracker signatures", "Graceful shutdown drains in-flight requests without data loss").
- Tracker config CLI: `docs/superpowers/specs/tracker/2026-04-25-tracker-internal-config-design.md` §3.3 (exit-code matrix).
- Ledger design: `docs/superpowers/specs/ledger/2026-04-22-ledger-design.md` §4.1 (chain-integrity contract).

**Prerequisites:** all four implemented tracker modules at HEAD on this branch — verify by running `make -C tracker test` and getting a green tree before starting. The new package adds no production code dependencies.

**Repo path note:** This worktree is rooted at `/home/user/token-bay`. All absolute paths in this plan use that prefix. If the user is operating from a different worktree path (e.g. `/Users/<name>/.superset/worktrees/token-bay/<branch>`), substitute that prefix throughout.

**Out of scope (defer to later plans):**
- Real listener / RPC e2e — needs `internal/server` + `internal/api` + `internal/session` (all empty today). The eventual plan: spin up the listener on `127.0.0.1:0`, connect a stubbed plugin client, drive `enroll → broker_request → usage_report → settle_request` against a real broker. This plan deliberately stays out of `internal/server` until that subsystem lands.
- Broker selection scoring — needs `internal/broker`. The composed-state harness today registers seeders and runs `registry.Match` (the candidate-set step of §5.1); the scoring step (α·reputation + β·headroom − γ·rtt − δ·load) is broker territory.
- Admin HTTP API — needs `internal/admin`. `GET /health`, `GET /stats`, peer-management endpoints all wait on that subsystem.
- Federation peer protocol — needs `internal/federation`. Peer-tracker handshake, Merkle-root gossip, cross-region transfers all deferred.
- STUN/TURN and NAT-traversal — needs `internal/stunturn`. The deferred test shapes (loopback STUN binding, loopback TURN relay round-trip, netns-based NAT-simulation matrix) are sketched in [Network surface — deferred test shapes](#network-surface--deferred-test-shapes-gated-on-internalstunturn) below; that section is the testing contract the eventual `internal/stunturn` plan should land against.
- Reputation freeze list — needs `internal/reputation`.
- The `make run-local` flow — `scripts/dev-run-local.sh` exits non-zero today by design (no `run` subcommand yet). This plan does not introduce one.

---

## Table of contents

1. [File map](#1-file-map)
2. [Conventions](#2-conventions)
3. [Task 1: Package scaffold + shared helpers + fixtures](#task-1-package-scaffold--shared-helpers--fixtures)
4. [Task 2: CLI version surface](#task-2-cli-version-surface)
5. [Task 3: CLI config validate — happy path](#task-3-cli-config-validate--happy-path)
6. [Task 4: CLI config validate — exit-code matrix](#task-4-cli-config-validate--exit-code-matrix)
7. [Task 5: Composed lifecycle — config → ledger → balance round-trip](#task-5-composed-lifecycle--config--ledger--balance-round-trip)
8. [Task 6: Composed lifecycle — registry candidate matching against config-derived shape](#task-6-composed-lifecycle--registry-candidate-matching-against-config-derived-shape)
9. [Task 7: Restart-survives-disk lifecycle + suite green check](#task-7-restart-survives-disk-lifecycle--suite-green-check)
10. [Network surface — deferred test shapes (gated on `internal/stunturn`)](#network-surface--deferred-test-shapes-gated-on-internalstunturn)

---

## 1. File map

Created in this plan:

| Path | Purpose |
|---|---|
| `tracker/test/e2e/doc.go` | Package documentation comment |
| `tracker/test/e2e/helpers_test.go` | Shared helpers (binary build cache, exec wrapper, temp data-dir builder, deterministic tracker key, composed-state opener) |
| `tracker/test/e2e/cli_test.go` | Tasks 2, 3, 4 — CLI surface |
| `tracker/test/e2e/lifecycle_test.go` | Tasks 5, 6, 7 — composed-state surface |
| `tracker/test/e2e/testdata/valid.yaml` | Minimal-but-complete tracker config pointing at `${data_dir}` paths the tests fill in at runtime |
| `tracker/test/e2e/testdata/invalid_unknown_field.yaml` | Surface-level fixture for parse-error exit-code 2 (we own this rather than reaching into `internal/config/testdata/`) |
| `tracker/test/e2e/testdata/invalid_score_weights.yaml` | Surface-level fixture for validation-error exit-code 3 |

Modified: none. The `tracker/test/e2e/` directory exists but is empty; this plan populates it. The plan does not change any production code, any existing test, the Makefile, or `go.mod`.

The plan does not import or touch fixtures under `tracker/internal/config/testdata/` even though equivalents exist there. The e2e package is meant to be testable in isolation — its fixtures live next to its tests. Reaching across module boundaries for testdata is a hidden coupling that breaks if `internal/config` reshuffles its fixtures.

---

## 2. Conventions

- All Go commands run from `/home/user/token-bay/tracker` unless stated. Use absolute paths in commands so the engineer can copy-paste.
- `PATH="$HOME/.local/share/mise/shims:$PATH"` may be required if Go is managed by mise. Each Bash step that runs `go` includes it for safety.
- One commit per task. Conventional-commit prefix: `test(tracker/e2e): <summary>`.
- Co-Authored-By footer (every commit): `Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>`.
- Test files use external test package `package e2e_test` so they consume only exported APIs from the modules under test — same boundary `internal/server` will have when it lands.
- All tests use `t.TempDir()` for filesystem state. No global state; tests must be safe under `-race` and `-parallel`.
- No build tag. These tests run on every `make -C tracker test`. Wall-time budget: < 3s for the full e2e package (the `go build` is cached in the helpers; per-test exec adds ~50ms each).
- Deterministic Ed25519 keys derived from labels via `sha256` — same pattern as `tracker/internal/ledger/helpers_test.go::trackerKeypair`. We re-derive locally rather than re-export the helper, because helpers under `_test.go` aren't visible across packages.

---

## Task 1: Package scaffold + shared helpers + fixtures

**Files:**
- Create: `tracker/test/e2e/doc.go`
- Create: `tracker/test/e2e/helpers_test.go`
- Create: `tracker/test/e2e/testdata/valid.yaml`
- Create: `tracker/test/e2e/testdata/invalid_unknown_field.yaml`
- Create: `tracker/test/e2e/testdata/invalid_score_weights.yaml`

The helpers file holds the scaffolding consumed by every test in subsequent tasks. By landing it in its own commit we get a clean reusable foundation — and the per-test commits below stay focused on the assertion they introduce.

- [ ] **Step 1: Create the package doc**

Create `tracker/test/e2e/doc.go`:

```go
// Package e2e contains end-to-end tests for the Token-Bay tracker.
//
// Each test drives the tracker through a public boundary that exists at
// HEAD: either the compiled `token-bay-tracker` CLI (binary surface) or
// the in-process composition of internal/config + internal/ledger +
// internal/registry along the same wiring path internal/server.Run will
// follow when that subsystem lands.
//
// As internal/server / internal/api / internal/admin become non-empty,
// the composed-state tests in this package graduate from "open subsystems
// in-process and drive them" to "spin up the listener and drive subsystems
// through the wire." The scenario coverage stays the same; the surface
// under test grows.
//
// Tests are hermetic: t.TempDir() filesystem, deterministic Ed25519
// keypairs, fixed clock for the ledger. No real plugins, no QUIC, no
// federation peers.
//
// Run with: make -C tracker test (no build tag required).
package e2e
```

- [ ] **Step 2: Create the helpers test file**

Create `tracker/test/e2e/helpers_test.go`. The helpers expose four primitives:

1. `buildTrackerBinary(t)` — `go build` once per `go test` invocation (cached via `sync.Once` against the package's tempdir), returns an absolute path to the compiled binary. Re-used by every CLI test.
2. `runTracker(t, binPath, args...)` — exec wrapper that returns stdout, stderr, and exit code with a 10-second context deadline. Captures both streams via `cmd.Output()`/`cmd.Stderr` buffers.
3. `newComposedState(t)` — builds a tracker `*config.Config` from `testdata/valid.yaml` rewritten to point at a fresh `t.TempDir()`, opens the ledger storage at the rewritten `ledger.storage_path`, opens a `*ledger.Ledger` over it with a deterministic tracker keypair and a fixed clock (`time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)`), constructs a `*registry.Registry` with `registry.DefaultShardCount`, and registers the close hooks via `t.Cleanup`. Returns a struct holding `(*config.Config, *ledger.Ledger, *registry.Registry, ed25519.PublicKey)` so each test can drive whichever subsystem it cares about.
4. `labeledIdentity(label)` — `sha256("e2e-identity-" + label)[:32]` — produces a deterministic 32-byte identity ID. The ledger doesn't validate that the bytes are an actual Ed25519 pubkey hash for STARTER_GRANT entries, so a sha256 stand-in is fine and reproducible.

The file's import list:

```go
import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
	"github.com/token-bay/token-bay/tracker/internal/registry"
)
```

Implementation sketch (write this in full when the task runs — abridged here for plan brevity):

```go
// Package-scoped binary cache so we go build at most once per test run.
var (
	binOnce sync.Once
	binPath string
	binErr  error
)

// buildTrackerBinary compiles tracker/cmd/token-bay-tracker into a
// throwaway tempdir and returns the absolute path. The compiled binary
// outlives all subtests in the package — first caller pays the build
// cost (~1.5s), subsequent callers no-op.
func buildTrackerBinary(t *testing.T) string {
	t.Helper()
	binOnce.Do(func() {
		dir, err := os.MkdirTemp("", "token-bay-tracker-e2e-bin-")
		if err != nil {
			binErr = err
			return
		}
		binPath = filepath.Join(dir, "token-bay-tracker")
		// `go build` from the tracker module root so go.work picks up
		// the local shared/ replace directive.
		cmd := exec.Command("go", "build", "-o", binPath,
			"github.com/token-bay/token-bay/tracker/cmd/token-bay-tracker")
		// Inherit the current env so GOPATH / mise shims / GOCACHE work.
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			binErr = fmt.Errorf("go build: %v\nstderr:\n%s", err, stderr.String())
		}
	})
	require.NoError(t, binErr)
	return binPath
}

type runResult struct {
	Stdout, Stderr string
	ExitCode       int
}

// runTracker execs the compiled tracker with the given args under a 10s
// context deadline. ExitCode is the process exit status (0 on success,
// the cobra-invoked exit hook code on failure).
func runTracker(t *testing.T, binPath string, args ...string) runResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	var outBuf, errBuf bytes.Buffer
	cmd := exec.CommandContext(ctx, binPath, args...)
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()

	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errorsAs(err, &exitErr) {
			t.Fatalf("runTracker(%q): %v\nstderr: %s", args, err, errBuf.String())
		}
		code = exitErr.ExitCode()
	}
	return runResult{Stdout: outBuf.String(), Stderr: errBuf.String(), ExitCode: code}
}

// errorsAs is a tiny wrapper so the snippet above compiles without
// adding "errors" to the import block — collapse into the real import
// list when implementing.
func errorsAs(err error, target any) bool { /* errors.As(err, target) */ }

type composedState struct {
	Cfg      *config.Config
	Ledger   *ledger.Ledger
	Registry *registry.Registry
	Pub      ed25519.PublicKey
	DataDir  string
}

// newComposedState rewrites testdata/valid.yaml against a fresh tempdir,
// opens ledger storage + ledger orchestrator + registry, and registers
// teardown via t.Cleanup. The clock is fixed at 2026-05-03 12:00:00 UTC
// so timestamps in entries and signed snapshots are reproducible across
// test runs.
func newComposedState(t *testing.T) *composedState {
	t.Helper()
	dataDir := t.TempDir()
	yamlPath := writeValidConfigInto(t, dataDir)

	cfg, err := config.Load(yamlPath)
	require.NoError(t, err)

	ctx := context.Background()
	store, err := storage.Open(ctx, cfg.Ledger.StoragePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	pub, priv := deterministicTrackerKeypair()
	clock := func() time.Time { return time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC) }
	led, err := ledger.Open(store, priv, ledger.WithClock(clock))
	require.NoError(t, err)
	t.Cleanup(func() { _ = led.Close() })

	reg, err := registry.New(registry.DefaultShardCount)
	require.NoError(t, err)

	return &composedState{Cfg: cfg, Ledger: led, Registry: reg, Pub: pub, DataDir: dataDir}
}

// writeValidConfigInto loads testdata/valid.yaml, substitutes every
// "/var/lib/token-bay" prefix with the test tempdir, and writes the
// result to a tempdir-scoped tracker.yaml. Returns the path.
func writeValidConfigInto(t *testing.T, dataDir string) string {
	t.Helper()
	template, err := os.ReadFile(filepath.Join("testdata", "valid.yaml"))
	require.NoError(t, err)
	rewritten := strings.ReplaceAll(string(template), "/var/lib/token-bay", dataDir)
	rewritten = strings.ReplaceAll(rewritten, "/etc/token-bay", dataDir)
	out := filepath.Join(dataDir, "tracker.yaml")
	require.NoError(t, os.WriteFile(out, []byte(rewritten), 0o600))
	return out
}

// deterministicTrackerKeypair derives an Ed25519 keypair from a fixed
// seed so tracker_sig bytes are reproducible across e2e runs. Mirrors
// the convention in tracker/internal/ledger/helpers_test.go without
// re-exporting (helpers in _test.go aren't visible cross-package).
func deterministicTrackerKeypair() (ed25519.PublicKey, ed25519.PrivateKey) {
	seed := sha256.Sum256([]byte("e2e-tracker-key"))
	priv := ed25519.NewKeyFromSeed(seed[:])
	return priv.Public().(ed25519.PublicKey), priv
}

// labeledIdentity returns a deterministic 32-byte identity ID for use in
// starter grants and registry registrations. The ledger does not require
// these bytes to be an actual Ed25519 pubkey hash for STARTER_GRANT
// entries (no counterparty signatures are verified), so a sha256
// stand-in is reproducible and sufficient.
func labeledIdentity(label string) ids.IdentityID {
	sum := sha256.Sum256([]byte("e2e-identity-" + label))
	return ids.IdentityID(sum)
}
```

Replace the `errorsAs` shim with a real `errors.As` import in the implementation; the abbreviation here keeps the plan readable.

- [ ] **Step 3: Create the valid YAML fixture**

Create `tracker/test/e2e/testdata/valid.yaml`. This is a self-contained fixture — every required field per design spec §4.2 is present, and the optional sections use spec defaults. The `/var/lib/token-bay` and `/etc/token-bay` prefixes are placeholders; `writeValidConfigInto` rewrites them to `t.TempDir()` at runtime so each test gets isolated paths. Use `tracker/internal/config/testdata/full.yaml` as the source of truth — copy verbatim, no modification.

- [ ] **Step 4: Create the unknown-field fixture**

Create `tracker/test/e2e/testdata/invalid_unknown_field.yaml`:

```yaml
# Triggers a parse error: server.zzz_unknown is not a recognized field,
# and config.Parse uses yaml.Decoder.KnownFields(true).
data_dir: /var/lib/token-bay
server:
  listen_addr: "0.0.0.0:7777"
  identity_key_path: /etc/token-bay/identity.key
  tls_cert_path: /etc/token-bay/cert.pem
  tls_key_path: /etc/token-bay/cert.key
  zzz_unknown: oops
ledger:
  storage_path: /var/lib/token-bay/ledger.sqlite
```

- [ ] **Step 5: Create the validation-error fixture**

Create `tracker/test/e2e/testdata/invalid_score_weights.yaml`. Mirror the invariant covered by `tracker/internal/config/testdata/invalid_score_weights_sum.yaml` — the four broker score weights must sum to 1.0. Setting them to `0.5/0.5/0.5/0.5` yields `2.0`, which validation rejects with a `FieldError` keyed by `broker.score_weights` (or `admission.score_weights` — pick one and stay consistent with the assertion in Task 4). Copy from `tracker/internal/config/testdata/invalid_score_weights_sum.yaml` if it covers the same invariant; otherwise hand-roll.

- [ ] **Step 6: Verify the package builds**

Run:

```bash
PATH="$HOME/.local/share/mise/shims:$PATH" go vet ./test/e2e/...
```

Expected: no output (success). Compile errors at this step are usually a renamed export from `internal/config`/`internal/ledger`/`internal/registry` (e.g. `ledger.WithClock` removed, `registry.DefaultShardCount` renamed). Fix the helper to match the current export name and re-vet.

- [ ] **Step 7: Run the (empty) test set to confirm wiring**

Run:

```bash
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./test/e2e/...
```

Expected: `ok ... [no test files]` (no `Test*` funcs yet — `helpers_test.go` only defines helpers). Exit 0.

- [ ] **Step 8: Commit**

```bash
git add tracker/test/e2e/doc.go tracker/test/e2e/helpers_test.go tracker/test/e2e/testdata/
git commit -m "$(cat <<'EOF'
test(tracker/e2e): scaffold + shared helpers + fixtures

Adds the tracker/test/e2e package with three reusable primitives — a
sync.Once-cached binary build, a runTracker exec wrapper that captures
stdout/stderr/exit, and a newComposedState opener that wires config →
ledger → registry against a tempdir-rewritten valid.yaml. Three fixture
YAMLs cover the happy path, the parse-error path, and the validation-
error path. Subsequent commits add the per-surface test cases.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: CLI version surface

**Files:**
- Create: `tracker/test/e2e/cli_test.go`

**What it proves:** the compiled binary's `version` subcommand prints the documented format. `cmd/token-bay-tracker/main_test.go` already covers this *in-process* (cobra command tree); this test covers it *out-of-process* (real exec, real stdout buffer, real exit code 0). Pins the binary's process boundary — including that `main()` doesn't `os.Exit(1)` from a misregistered subcommand path.

- [ ] **Step 1: Write the failing test**

Create `tracker/test/e2e/cli_test.go`:

```go
package e2e_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCLI_VersionPrintsExpected: `token-bay-tracker version` writes
// "token-bay-tracker <version>\n" to stdout and exits 0.
func TestCLI_VersionPrintsExpected(t *testing.T) {
	bin := buildTrackerBinary(t)
	res := runTracker(t, bin, "version")

	assert.Equal(t, 0, res.ExitCode, "stderr: %s", res.Stderr)
	assert.Contains(t, res.Stdout, "token-bay-tracker")
	assert.Contains(t, res.Stdout, "0.0.0-dev")
	assert.Empty(t, res.Stderr)
}
```

The version constant `0.0.0-dev` lives in `cmd/token-bay-tracker/main.go`. If a future commit bumps it, this assertion breaks deliberately — the e2e suite is the right place to catch a version-format regression.

- [ ] **Step 2: Run the test**

Run:

```bash
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -run TestCLI_VersionPrintsExpected ./test/e2e/...
```

Expected: PASS. First run pays the `go build` cost (~1.5s); subsequent runs reuse the cached binary.

- [ ] **Step 3: Commit**

```bash
git add tracker/test/e2e/cli_test.go
git commit -m "$(cat <<'EOF'
test(tracker/e2e): version CLI surface

Compile the binary and exec `token-bay-tracker version`; assert exit 0,
expected stdout, no stderr. Out-of-process counterpart to the existing
in-process test in cmd/token-bay-tracker/main_test.go — pins the binary's
process boundary.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: CLI config validate — happy path

**Files:**
- Modify: `tracker/test/e2e/cli_test.go`

**What it proves:** the binary's `config validate` subcommand loads a fully-specified config from disk, runs the full `Parse → ApplyDefaults → Validate` chain inside `config.Load`, and reports success. The fixture is rewritten against a `t.TempDir()` so the paths in the config (`identity_key_path`, `tls_cert_path`, etc.) refer to nonexistent-but-plausible locations — `Validate` does not stat those paths (per `tracker/internal/config/CLAUDE.md` "Validate touches the filesystem exactly once: tlog_path parent-dir existence"), so the test is hermetic.

- [ ] **Step 1: Append the failing test**

Append to `tracker/test/e2e/cli_test.go`:

```go
import (
	"os"
	"path/filepath"
	"strings"

	"github.com/stretchr/testify/require"
)

// TestCLI_ConfigValidate_HappyPath: a complete tracker config rewritten
// against a tempdir loads, validates, and reports OK with section count.
func TestCLI_ConfigValidate_HappyPath(t *testing.T) {
	dataDir := t.TempDir()
	tmpl, err := os.ReadFile(filepath.Join("testdata", "valid.yaml"))
	require.NoError(t, err)
	rewritten := strings.ReplaceAll(string(tmpl), "/var/lib/token-bay", dataDir)
	rewritten = strings.ReplaceAll(rewritten, "/etc/token-bay", dataDir)
	cfgPath := filepath.Join(dataDir, "tracker.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(rewritten), 0o600))

	bin := buildTrackerBinary(t)
	res := runTracker(t, bin, "config", "validate", "--config", cfgPath)

	assert.Equal(t, 0, res.ExitCode, "stderr: %s", res.Stderr)
	assert.Contains(t, res.Stdout, "OK")
	assert.Contains(t, res.Stdout, cfgPath)
	assert.Empty(t, res.Stderr)
}
```

If consolidating imports, collapse the new `os`/`filepath`/`strings`/`require` into the file's existing import block — Go forbids adjacent `import (...)` clauses without intervening declarations.

- [ ] **Step 2: Run the test**

Run:

```bash
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -run "TestCLI_VersionPrintsExpected|TestCLI_ConfigValidate_HappyPath" ./test/e2e/...
```

Expected: both PASS.

- [ ] **Step 3: Commit**

```bash
git add tracker/test/e2e/cli_test.go
git commit -m "$(cat <<'EOF'
test(tracker/e2e): config validate CLI happy path

Rewrite testdata/valid.yaml against t.TempDir(), exec
`token-bay-tracker config validate --config <path>`, assert exit 0 and
"OK <path>" stdout. Pins the binary's full Parse → ApplyDefaults →
Validate chain end-to-end at the process boundary.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: CLI config validate — exit-code matrix

**Files:**
- Modify: `tracker/test/e2e/cli_test.go`

**What it proves:** the binary maps the three classes of `config.Load` errors to the spec-defined exit codes (design spec §3.3): missing/unreadable file → 1, parse error → 2, validation error → 3. This is the contract operators script against (deployment systems test exit codes, not stderr scraping). The in-process test in `cmd/token-bay-tracker/config_cmd_test.go` covers it via the context-based exit hook; this test re-asserts the contract through the actual `os.Exit` boundary.

- [ ] **Step 1: Append three table-driven cases**

Append to `tracker/test/e2e/cli_test.go`:

```go
// TestCLI_ConfigValidate_ExitCodeMatrix: the three classes of
// config.Load failure map to the spec-defined exit codes per
// docs/superpowers/specs/tracker/2026-04-25-tracker-internal-config-design.md
// §3.3.
func TestCLI_ConfigValidate_ExitCodeMatrix(t *testing.T) {
	bin := buildTrackerBinary(t)

	cases := []struct {
		name           string
		args           []string
		wantExit       int
		wantStderrSub  string
	}{
		{
			name:          "missing_file",
			args:          []string{"config", "validate", "--config", "/nonexistent/tracker.yaml"},
			wantExit:      1,
			wantStderrSub: "no such file",
		},
		{
			name:          "missing_flag",
			args:          []string{"config", "validate"},
			wantExit:      1,
			wantStderrSub: "--config is required",
		},
		{
			name:          "unknown_field",
			args:          []string{"config", "validate", "--config", filepath.Join("testdata", "invalid_unknown_field.yaml")},
			wantExit:      2,
			wantStderrSub: "parse",
		},
		{
			name:          "validation_failure",
			args:          []string{"config", "validate", "--config", filepath.Join("testdata", "invalid_score_weights.yaml")},
			wantExit:      3,
			wantStderrSub: "validation",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runTracker(t, bin, tc.args...)
			assert.Equal(t, tc.wantExit, res.ExitCode, "stdout: %s\nstderr: %s", res.Stdout, res.Stderr)
			assert.Contains(t, strings.ToLower(res.Stderr), tc.wantStderrSub)
		})
	}
}
```

The `wantStderrSub` checks are intentionally loose substring matches against lowercased stderr — the exact phrasing is the binary's, not the spec's, and is allowed to drift. The exit code is the operator-visible contract; a stderr-format change should not break this test.

If `invalid_score_weights.yaml` and the binary's stderr key the validation error to `admission.score_weights` rather than `broker.score_weights`, that's fine — the assertion only checks for the substring "validation", not the field name. A future test could add a stricter field-name check; today's tightest expectation is the exit code.

- [ ] **Step 2: Run the full CLI suite**

Run:

```bash
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -run TestCLI ./test/e2e/...
```

Expected: PASS for every subtest.

- [ ] **Step 3: Commit**

```bash
git add tracker/test/e2e/cli_test.go
git commit -m "$(cat <<'EOF'
test(tracker/e2e): config validate exit-code matrix

Drive `token-bay-tracker config validate` against four failure modes —
missing file, missing flag, unknown field, validation failure — and
assert the spec §3.3 exit codes (1, 1, 2, 3). Pins the operator-visible
contract scripts depend on, through the real os.Exit boundary rather
than the cobra exit hook used by the unit test.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: Composed lifecycle — config → ledger → balance round-trip

**Files:**
- Create: `tracker/test/e2e/lifecycle_test.go`

**What it proves:** the wiring path the eventual `internal/server.Run` will execute on every cold start — load YAML → open SQLite over `ledger.storage_path` → open the ledger orchestrator with the tracker keypair → issue a starter grant → query the signed balance — composes correctly today. Specifically:

1. `config.Load` produces a `*Config` whose `Ledger.StoragePath` is a writable destination.
2. `storage.Open` followed by `ledger.Open` succeeds on a previously-empty SQLite file at that path.
3. `Ledger.IssueStarterGrant` persists an entry that immediately satisfies `Ledger.SignedBalance` for the same identity, with the documented credit amount.
4. The resulting snapshot validates as well-formed (`Body.Credits`, `Body.IdentityId`, `TrackerSig` present).
5. `Ledger.AssertChainIntegrity(0, 0)` passes after the round-trip.

This is the smallest meaningful "tracker lifecycle" expressible without `internal/server`. It does not exercise the wire form; it exercises the boundary contract between modules that the server will compose.

- [ ] **Step 1: Write the failing test**

Create `tracker/test/e2e/lifecycle_test.go`:

```go
package e2e_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLifecycle_StarterGrantThenBalance: load config, open ledger, issue
// a starter grant for a labeled identity, then query SignedBalance for
// the same identity — asserts the credit amount round-trips and chain
// integrity holds.
func TestLifecycle_StarterGrantThenBalance(t *testing.T) {
	state := newComposedState(t)
	ctx := context.Background()

	consumerID := labeledIdentity("consumer-1")
	idBytes := consumerID.Bytes()

	const grant = uint64(500)
	entry, err := state.Ledger.IssueStarterGrant(ctx, idBytes, grant)
	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.Equal(t, uint64(1), entry.Body.Seq, "first entry on a fresh chain has seq=1")

	snap, err := state.Ledger.SignedBalance(ctx, idBytes)
	require.NoError(t, err)
	require.NotNil(t, snap)
	require.NotNil(t, snap.Body)
	assert.Equal(t, grant, snap.Body.Credits)
	assert.Equal(t, idBytes, snap.Body.IdentityId)
	assert.NotEmpty(t, snap.TrackerSig, "tracker must sign every snapshot")

	require.NoError(t, state.Ledger.AssertChainIntegrity(ctx, 0, 0))
}
```

- [ ] **Step 2: Run the test**

Run:

```bash
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -run TestLifecycle_StarterGrantThenBalance ./test/e2e/...
```

Expected: PASS. The fixed clock injected by `newComposedState` ensures `IssuedAt` / `ExpiresAt` on the snapshot are deterministic, but this test does not assert on them — it asserts only on the fields the operator-facing scenario depends on (credits, identity, signature presence, chain integrity).

If the snapshot's `Body.Credits` is zero, the most likely cause is that `Ledger.SignedBalance` is reading a balance projection that hasn't been refreshed after the append — debug there before adjusting the test. The chain-integrity assertion is a separate sanity check; if it fails, the failure is in `internal/ledger` and out of scope for this commit.

- [ ] **Step 3: Commit**

```bash
git add tracker/test/e2e/lifecycle_test.go
git commit -m "$(cat <<'EOF'
test(tracker/e2e): config-to-ledger-to-balance round-trip

Compose internal/config + internal/ledger + internal/ledger/storage
along the wiring path internal/server.Run will follow: load valid.yaml
against a tempdir, open SQLite at ledger.storage_path, open the
orchestrator with a deterministic tracker key, issue a starter grant,
query SignedBalance, assert credits round-trip and chain integrity holds.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: Composed lifecycle — registry candidate matching against config-derived shape

**Files:**
- Modify: `tracker/test/e2e/lifecycle_test.go`

**What it proves:** the registry, sized from the tracker's config, holds enough information to support the broker's §5.1 candidate-set step. A test "broker" — implemented as a `registry.Filter` literal in the test, not a real broker — derives its filter shape from the loaded `*config.Config` (`MinHeadroom = cfg.Broker.HeadroomThreshold`, `MaxLoad = cfg.Broker.LoadThreshold`) and queries a registry seeded with three seeders chosen to exercise both filter passes and filter rejects.

This is the largest cross-module composition expressible without `internal/broker`. When `internal/broker` lands, the test reads the same fixture seeders, constructs a real broker over the registry, and asserts on selection — the registry-half of this test stays as-is.

- [ ] **Step 1: Append the failing test**

Append to `tracker/test/e2e/lifecycle_test.go`:

```go
import (
	"net/netip"
	"time"

	"github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/registry"
)

// TestLifecycle_RegistryMatchUsesConfigDerivedFilter: seed the registry
// with three seeders (two viable, one below the config-derived headroom
// floor), construct a Filter from cfg.Broker.{HeadroomThreshold,
// LoadThreshold}, and assert Match returns exactly the two viable ones.
//
// Establishes that internal/registry's candidate-set step (broker spec
// §5.1, registry-side) is self-consistent with the values an operator
// configures in tracker.yaml.
func TestLifecycle_RegistryMatchUsesConfigDerivedFilter(t *testing.T) {
	state := newComposedState(t)
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)

	// Three seeders, all available on claude-opus-4-7.
	viable1 := labeledIdentity("seeder-viable-1")
	viable2 := labeledIdentity("seeder-viable-2")
	belowHeadroom := labeledIdentity("seeder-below-headroom")

	caps := registry.Capabilities{
		Models:     []string{"claude-opus-4-7"},
		MaxContext: 200_000,
		Tiers:      []proto.PrivacyTier{proto.PrivacyTier_PRIVACY_TIER_STANDARD},
	}
	addr := netip.MustParseAddrPort("198.51.100.1:7777")

	for _, id := range []ids.IdentityID{viable1, viable2, belowHeadroom} {
		state.Registry.Register(registry.SeederRecord{
			IdentityID:    id,
			LastHeartbeat: now,
			NetCoords:     registry.NetCoords{ExternalAddr: addr},
		})
	}
	require.NoError(t, state.Registry.Advertise(viable1, caps, true, 0.8))
	require.NoError(t, state.Registry.Advertise(viable2, caps, true, 0.5))
	// Headroom threshold from config defaults is 0.2 — set below it.
	require.NoError(t, state.Registry.Advertise(belowHeadroom, caps, true, 0.05))

	filter := registry.Filter{
		RequireAvailable: true,
		Model:            "claude-opus-4-7",
		Tier:             proto.PrivacyTier_PRIVACY_TIER_STANDARD,
		MinHeadroom:      state.Cfg.Broker.HeadroomThreshold,
		MaxLoad:          state.Cfg.Broker.LoadThreshold,
	}

	matched := state.Registry.Match(filter)

	gotIDs := map[ids.IdentityID]bool{}
	for _, rec := range matched {
		gotIDs[rec.IdentityID] = true
	}
	assert.Len(t, matched, 2, "expected exactly the two viable seeders to match")
	assert.True(t, gotIDs[viable1], "viable-1 should match")
	assert.True(t, gotIDs[viable2], "viable-2 should match")
	assert.False(t, gotIDs[belowHeadroom], "below-headroom seeder must not match (cfg headroom=%.2f)", state.Cfg.Broker.HeadroomThreshold)
}
```

Collapse the new imports into the file's existing import block when implementing.

The test deliberately uses the *config-derived* threshold rather than a hardcoded number — that's the e2e-shaped property it asserts. If the operator changes `broker.headroom_threshold`, the same test scenario routes the same set of seeders. (The viable seeders are above any threshold ≤ 0.5; the below-headroom seeder is below any threshold ≥ 0.1 — so the test's assertion is robust to default-value tweaks within a sensible range.)

- [ ] **Step 2: Run the test**

Run:

```bash
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race -run TestLifecycle ./test/e2e/...
```

Expected: PASS for both lifecycle tests.

- [ ] **Step 3: Commit**

```bash
git add tracker/test/e2e/lifecycle_test.go
git commit -m "$(cat <<'EOF'
test(tracker/e2e): registry candidate matching against config-derived filter

Seed the registry with three seeders, build a registry.Filter from
cfg.Broker.{HeadroomThreshold, LoadThreshold}, assert Match returns the
two seeders above the config's headroom floor and rejects the one below.
Pins the registry side of broker spec §5.1 against operator-tunable
config — when internal/broker lands, the same fixture composes against
a real broker.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: Restart-survives-disk lifecycle + suite green check

**Files:**
- Modify: `tracker/test/e2e/lifecycle_test.go`

**What it proves:** the tracker spec §11 acceptance "Graceful shutdown drains in-flight requests without data loss" decomposes — at the persistence layer — into "ledger entries durable across orchestrator restart." This test exercises that invariant end-to-end through the public `internal/ledger` and `internal/ledger/storage` APIs: append entries on a Ledger, close it and its store, reopen both from the same on-disk path the YAML config pointed at, and confirm `Tip()` and `AssertChainIntegrity()` both reflect the prior state.

`tracker/internal/ledger/integration_test.go::TestIntegration_RestartPreservesChainIntegrity` already covers this *inside* the ledger package using its private helpers. This test re-asserts it from *outside* — through the same path-routing the operator's config drives — so an unintentional inversion of `cfg.Ledger.StoragePath` and the orchestrator's `storage.Open` arg would surface here even if the package-internal test stayed green.

- [ ] **Step 1: Append the failing test**

Append to `tracker/test/e2e/lifecycle_test.go`:

```go
// TestLifecycle_RestartPreservesChainIntegrity: open the ledger over
// cfg.Ledger.StoragePath, append entries, close everything, reopen from
// the same path, and confirm tip + chain integrity survive. Operator-
// shaped variant of the existing internal/ledger integration test;
// catches a path-wiring regression that wouldn't surface in-package.
func TestLifecycle_RestartPreservesChainIntegrity(t *testing.T) {
	state := newComposedState(t)
	ctx := context.Background()
	idBytes := labeledIdentity("consumer-restart").Bytes()

	for range 3 {
		_, err := state.Ledger.IssueStarterGrant(ctx, idBytes, 100)
		require.NoError(t, err)
	}

	tipSeqBefore, _, _, err := state.Ledger.Tip(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(3), tipSeqBefore)

	// Close orchestrator + store explicitly so the WAL flushes. The
	// t.Cleanup hooks set up by newComposedState would also call these,
	// but ordering matters here: we need the close to happen before the
	// reopen, so we drive it manually and unregister the cleanups by
	// no-oping closes (Close is idempotent — calling it twice is safe).
	require.NoError(t, state.Ledger.Close())

	// Reopen against the same on-disk path cfg pointed at.
	store2, err := storage.Open(ctx, state.Cfg.Ledger.StoragePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store2.Close() })

	_, priv := deterministicTrackerKeypair()
	led2, err := ledger.Open(store2, priv)
	require.NoError(t, err)

	tipSeqAfter, _, _, err := led2.Tip(ctx)
	require.NoError(t, err)
	assert.Equal(t, tipSeqBefore, tipSeqAfter, "tip must survive restart")
	require.NoError(t, led2.AssertChainIntegrity(ctx, 0, 0))
}
```

If `Ledger.Close()` is not idempotent today (the ledger doc.go says it's a no-op in v1), the manual `state.Ledger.Close()` plus the eventual `t.Cleanup`-driven close is fine. If a future v2 close acquires resources, revisit this test's ordering.

The test re-derives the tracker keypair via `deterministicTrackerKeypair()` rather than reusing the keypair from the composed state. The orchestrator stores no keypair on disk — it accepts whatever key the operator passes at `Open`. So the second `ledger.Open` must use the same deterministic key, otherwise `tracker_sig` verification on the existing chain would fail. Asserting that the same key recovers the chain pins the operator's "tracker key is loaded from cfg.Server.IdentityKeyPath" contract, even though that path-loading isn't exercised here (no key-file format yet — the test's bytewise key stand-in is the closest expressible thing today).

- [ ] **Step 2: Run the full e2e suite**

Run:

```bash
PATH="$HOME/.local/share/mise/shims:$PATH" go test -race ./test/e2e/...
```

Expected: PASS for all seven tests:
- `TestCLI_VersionPrintsExpected` (Task 2)
- `TestCLI_ConfigValidate_HappyPath` (Task 3)
- `TestCLI_ConfigValidate_ExitCodeMatrix` (Task 4 — four subtests)
- `TestLifecycle_StarterGrantThenBalance` (Task 5)
- `TestLifecycle_RegistryMatchUsesConfigDerivedFilter` (Task 6)
- `TestLifecycle_RestartPreservesChainIntegrity` (Task 7)

- [ ] **Step 3: Run the full tracker test suite under -race**

Run:

```bash
PATH="$HOME/.local/share/mise/shims:$PATH" make -C /home/user/token-bay/tracker test
```

Expected: every unit test, integration test, and e2e test PASS, no race-detector warnings, exit 0. The e2e package adds < 3s to the suite (binary build dominates; the seven tests are sub-100ms each after that).

- [ ] **Step 4: Run the linter**

Run:

```bash
PATH="$HOME/.local/share/mise/shims:$PATH" make -C /home/user/token-bay/tracker lint
```

Expected: no findings. Common false positives in the new files:
- `errcheck` on `_ = store2.Close()` in `t.Cleanup` — keep the underscore, it's intentional.
- `revive:exported` on the unexported `composedState` / `runResult` / `runTracker` helpers — they're test-only and unexported; the linter should not flag them. If it does, the package's `.golangci.yml` exclusion for `_test.go` files is missing — fix that in a separate `chore(tracker):` commit, not here.

- [ ] **Step 5: Commit**

```bash
git add tracker/test/e2e/lifecycle_test.go
git commit -m "$(cat <<'EOF'
test(tracker/e2e): chain integrity survives orchestrator restart

Append entries, close the orchestrator, reopen storage + a fresh
orchestrator against the same cfg.Ledger.StoragePath and tracker key,
assert tip and chain integrity round-trip. Operator-shaped variant of
the existing internal/ledger integration test — catches path-wiring
regressions that wouldn't surface in-package. Closes the e2e suite for
the three implemented tracker subsystems (config, ledger, registry).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Self-review notes (for the implementing engineer)

These notes record the cross-spec consistency checks already done while writing the plan. If a divergence appears at implementation time, prefer matching the actual code over what's written here.

- `config.Load` is the right entry point: `Parse → ApplyDefaults → Validate`, returns `*Config` whose section types match `tracker/internal/config/config.go`. `BrokerConfig.HeadroomThreshold` and `LoadThreshold` are real fields on this branch; verify before relying on them in Task 6.
- `registry.New(numShards int) (*Registry, error)` and `registry.DefaultShardCount` are the real exports (verified in `tracker/internal/registry/registry.go`). `SeederRecord.LastHeartbeat` is a `time.Time`; pass `time.Date(...)`, not a `time.Time{}` zero value, since `Match` won't filter on staleness today but will once a sweep lands and the tests should pre-empt that.
- `ledger.Open(store, priv, opts...) (*Ledger, error)` and `ledger.WithClock(now func() time.Time)` are the real signatures (verified in `tracker/internal/ledger/ledger.go`). The orchestrator does not own the Store — the test cleans them up independently.
- `Ledger.IssueStarterGrant(ctx, identityID []byte, amount uint64) (*tbproto.Entry, error)` requires `len(identityID) == 32`. `ids.IdentityID(...).Bytes()` returns a `[]byte` of exactly 32. Verified.
- `Ledger.SignedBalance(ctx, identityID []byte) (*tbproto.SignedBalanceSnapshot, error)` returns a snapshot whose `Body.Credits`, `Body.IdentityId`, and `TrackerSig` are populated for any identity with non-empty entries. The fixed clock injected via `WithClock` makes `IssuedAt`/`ExpiresAt` deterministic — this plan does not assert on those, but a future test could.
- `tbproto.SignedBalanceSnapshot.TrackerSig` field name should be verified against `shared/proto/balance.pb.go` on this branch — if it's a different name (e.g. `Sig`, `Signature`), update the assertion in Task 5.
- `Ledger.AssertChainIntegrity(ctx, sinceSeq, untilSeq uint64) error` with `(0, 0)` audits the full chain. Both bounds inclusive of zero is documented in `tracker/internal/ledger/integration_test.go::TestAssertChainIntegrity_EmptyChain`.
- `os/exec.ExitError.ExitCode()` returns -1 on signal-killed processes; on a normal cobra-driven `os.Exit(N)` it returns N. Tests on the four-case exit-code matrix should reliably observe the spec exit codes.
- `cmd/token-bay-tracker/main.go` registers `os.Exit` on the context via `withExitFunc(context.Background(), os.Exit)`. The CLI tests therefore observe real `os.Exit` codes, not `cobra` internal error codes.
- The e2e tests use `package e2e_test` (external test package). Helpers in `_test.go` files are visible only inside the package — that's why the binary-build cache, `runTracker`, and `newComposedState` all live in `helpers_test.go`, not in a separate `helpers/` subpackage that other tests could import.
- Wall-time budget: the helpers' `sync.Once` guards `go build`, so the cost is paid once per `go test` run regardless of subtest count. On a warm `go build` cache the build is sub-second; on cold cache it's ~2s. The e2e suite stays under 3s wall time on a typical dev machine. The full `make -C tracker test` should remain well under its existing ceiling.

## Network surface — deferred test shapes (gated on `internal/stunturn`)

The tracker spec §5.4 (STUN reflection + TURN relay) and acceptance criterion §11 ("STUN hole-punching succeeds on ≥ 80% of consumer-seeder pairs with common home NATs. TURN fallback on the remainder.") describe a network-surface contract that this plan deliberately does not exercise — `internal/stunturn` is empty at HEAD. There's no UDP listener to bind a STUN request against, no relay to allocate a session through.

This section sketches the three test shapes the eventual `internal/stunturn` plan should land alongside its production code. They aren't tasks in this plan; they're a contract so the stunturn plan has something concrete to build against rather than re-deriving the testing strategy from the spec.

### A. STUN reflector — loopback binding test

**Surface:** the tracker's STUN-like reflector (spec §5.4). Refreshes seeder `external_addr` on each heartbeat; consumers also call `stun_allocate()` at request time to learn their own reflexive address.

**Test shape:** spin the tracker's UDP listener on `127.0.0.1:0`, send a STUN binding request from a UDP socket bound to a different loopback port, assert the response's `XOR-MAPPED-ADDRESS` carries the request socket's `127.0.0.1:<source-port>` (not a hardcoded value, not the listener's own address). Hermetic — no NAT, no namespaces, ~10ms wall.

**Property under test:** the reflector reflects the *source* address it observes off the inbound packet, exactly. A bug that hardcoded the listener's address, swapped src/dst, or returned a stale cached address all fail this.

**File:** `tracker/test/e2e/stun_test.go`. New helpers: `dialUDPLoopback(t)` returning a `*net.UDPConn` on a fresh port, `sendBindingRequest(t, conn, srvAddr)` returning the parsed STUN response. Build tag: none (loopback works on every platform).

### B. TURN relay — loopback round-trip + per-seeder rate limit

**Surface:** lazy-allocated TURN relay per `request_id` (spec §5.4). Both sides connect with a session token; the tracker rate-limits relay allocations per seeder to prevent bandwidth abuse.

**Test shape — round-trip:** two stub clients (consumer + seeder), each on its own UDP socket, both call `turn_relay_open(session_id)` on the tracker; the consumer writes N bytes; the seeder reads them out byte-identical. Reverse direction too (full duplex). N spans a small message and a multi-MTU stream so a misset MTU cap surfaces.

**Test shape — rate limit:** a single seeder allocates relay sessions in a loop; assert the (limit+1)th allocation rejects with the documented error code. Verifies the spec's "rate-limited per seeder to prevent bandwidth abuse" without needing real bandwidth.

**File:** `tracker/test/e2e/turn_relay_test.go`. The plugin-stub side reuses the broker-RPC helper that the listener-e2e plan will land — TURN allocation goes through the same long-lived plugin connection, not a separate port. Build tag: none.

### C. Hole-punching across simulated NATs

**Surface:** the §11 acceptance ("≥ 80% of consumer-seeder pairs with common home NATs"). Pure-loopback tests cannot cover this because there is no NAT translation on `127.0.0.1` — the source address is the source address. To exercise the hole-punching protocol we need actual NAT semantics in the loop.

**Test shape:** Linux network namespaces with userspace NAT rules. Three namespaces: `consumer-NAT`, `seeder-NAT`, and the tracker on the public side. `nft` rules give each NAT a personality from the spec's "common home NAT" set: full-cone, restricted-cone, port-restricted-cone, symmetric. Each test cell:

1. Stand up the topology (consumer → consumer-NAT → public ↔ tracker, seeder → seeder-NAT → public ↔ tracker).
2. Drive `enroll → broker_request → seeder_assignment → STUN exchange → direct UDP send` between consumer and seeder *through* their NATs.
3. Assert the bytes arrive *without* falling back to TURN (i.e. the direct path won).

Run a matrix over the cross-product of NAT personalities. Assert the spec's 80% bar holds across the documented set. Aggregate per-cell success/failure into a CSV under `coverage/nat-sim.csv` so regressions are diff-reviewable.

**Constraints:**
- **Linux-only.** Build tag `//go:build nat_sim` excludes from `make test`. Runs in a dedicated CI job (`make -C tracker test-nat-sim`) on a Linux runner. macOS dev loops still get green `make test`.
- **Requires CAP_NET_ADMIN** for `ip netns` and `nft`. The make target documents this; the test fails fast with a clear `t.Skip("requires CAP_NET_ADMIN; run inside privileged CI job")` + diagnostic if the capability is missing — never silently passes.
- **Wall time:** ~30s per matrix cell, ~5min for the full matrix. Don't gate every PR on it; gate the merge queue (or a nightly job) on it.
- **Topology setup is infrastructure.** Lives in `tracker/test/e2e/natsim/topology.go` (package `natsim`, build-tagged). Test files import it; the matrix-driver test is `natsim/holepunch_test.go`.

**File:** `tracker/test/e2e/natsim/` (subdirectory because the build tag changes the package's compile set; keeping it in the parent package would force every reader to think about the tag).

### D. What the network surface still cannot cover (real NATs)

Real-world NAT-traversal success against actual home routers (CGNATs, residential CPEs, mobile-carrier NATs) is a conformance suite, not e2e. It needs cloud VMs with controlled-NAT egress configurations, and probably a small fleet of physical CPEs in a lab. Defer to a `plan(tracker): nat-conformance-suite` once we have a deployment to run it from. The §11 acceptance is met by the netns matrix in shape C plus periodic conformance runs against the lab fleet — neither alone is sufficient.

### Relationship to this plan

When the stunturn plan lands:

- Shapes A and B drop into `tracker/test/e2e/` alongside the existing `cli_test.go` / `lifecycle_test.go`. They reuse `helpers_test.go::buildTrackerBinary` and `newComposedState`, gaining a new `startStunTurn(state)` helper that the stunturn plan introduces.
- Shape C lands as its own subdirectory + build tag + Make target. The stunturn plan introduces `make test-nat-sim` and the CI workflow that runs it.
- Tasks 1–7 of this plan stay green throughout — none of them touch the network surface, so they do not need to be revisited.

---

## What this plan does not unlock

If a downstream consumer asks "can the tracker now serve real plugin connections?", the answer is no — that's still gated on `internal/server` + `internal/api` + `internal/session` landing. This plan adds the test scaffolding so that, when those subsystems land, the e2e tests can grow incrementally:

1. The first `internal/server.Run` plan adds a "spin up the listener" helper to `helpers_test.go` and converts `newComposedState` to also start the listener. Existing lifecycle tests run unchanged but now exercise the listener-fronted boundary.
2. The first `internal/broker` plan replaces the hand-rolled `registry.Filter` in Task 6 with a real `broker.Select` call.
3. The first stubbed-plugin-client plan adds a `dialPlugin` helper that opens a QUIC connection and drives `enroll → broker_request → ...` — at which point the e2e suite genuinely matches the description in `tracker/CLAUDE.md` ("end-to-end with stubbed plugin clients").
4. The first `internal/stunturn` plan picks up the network surface per [Network surface — deferred test shapes](#network-surface--deferred-test-shapes-gated-on-internalstunturn) above.

Each of those is a separate plan. This one lands the foundation.

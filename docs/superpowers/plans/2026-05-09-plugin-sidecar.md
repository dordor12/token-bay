# plugin/internal/sidecar — Supervisor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement `plugin/internal/sidecar` — the long-lived supervisor that composes the existing subsystems (config, identity, auditlog, trackerclient, ccproxy) into the running `token-bay-sidecar` process, plus wire a `run --config <path>` subcommand into the CLI entry point.

**Architecture:** The sidecar package is a thin composition root. `New(Deps)` constructs every subsystem; `Run(ctx)` starts them concurrently under an `errgroup` and blocks until ctx is cancelled or any subsystem fails, then performs ordered shutdown. No business logic lives here — slash commands, hook ingestion, network-mode coordination, and the offer/settlement push handlers are deliberately deferred to follow-on plans (their `Deps` fields are nil-able and start as no-ops). The cmd layer (`cmd/token-bay-sidecar/run_cmd.go`) loads `internal/config`, opens identity, builds the trackerclient endpoint list, and hands the whole bundle to `sidecar.New`.

**Tech Stack:** Go 1.25, `golang.org/x/sync/errgroup`, `rs/zerolog`, `spf13/cobra`. Stdlib for signal handling and atomic file paths. All wiring uses existing in-repo packages — no new external deps.

**Spec sources:**
- `docs/superpowers/specs/plugin/2026-04-22-plugin-design.md` §2.1, §2.5, §3 (sidecar = "long-running background service that maintains the long-lived tracker connection"; surfaces state through slash commands and hooks).
- Existing module APIs: `internal/config.Load`, `internal/identity.Open` + `Signer`, `internal/auditlog.Open`, `internal/trackerclient.New/Start/Close/Status`, `internal/ccproxy.New/Start/Shutdown`.

**Out of scope (clearly deferred to follow-on plans):**
- Slash-command IPC server. The sidecar exposes a `Status()` method but does not yet listen on a Unix socket for `/token-bay status` etc.
- Hook ingestion endpoint. `internal/hooks/` is empty; that plan owns the IPC contract.
- Network-mode coordinator (settings.json Enter/Exit driven by hook payloads).
- OfferHandler / SettlementHandler push acceptors (left nil; trackerclient tolerates nil per `reconnect.go:121`).
- Real bootstrap-list resolver for `tracker: auto` — cmd layer rejects `auto` with a clear "not yet implemented" error.
- The tunnel-mode network router (`ccproxy.NetworkRouter` already ships as a v1 501 stub).

---

## File Structure

**Create:**

| Path | Purpose |
|---|---|
| `plugin/internal/sidecar/doc.go` | Package documentation |
| `plugin/internal/sidecar/errors.go` | Error sentinels (`ErrInvalidDeps`, `ErrAlreadyStarted`, `ErrClosed`) |
| `plugin/internal/sidecar/errors_test.go` | Verifies sentinels are distinct, exported, and `errors.Is`-friendly |
| `plugin/internal/sidecar/deps.go` | `Deps` struct + `Validate()` |
| `plugin/internal/sidecar/deps_test.go` | Validate happy path + each required-field failure |
| `plugin/internal/sidecar/sidecar.go` | `App` type, `New`, `Run`, `Close` |
| `plugin/internal/sidecar/sidecar_test.go` | Lifecycle tests with stub subsystems |
| `plugin/internal/sidecar/status.go` | `Status` snapshot type + `App.Status()` |
| `plugin/internal/sidecar/status_test.go` | Snapshot reflects subsystem state |
| `plugin/internal/sidecar/CLAUDE.md` | Module-level CLAUDE.md (matches sibling-module convention) |
| `plugin/cmd/token-bay-sidecar/run_cmd.go` | `run --config <path>` subcommand |
| `plugin/cmd/token-bay-sidecar/run_cmd_test.go` | Cobra-level tests: missing flag, bad config path, `tracker: auto` refusal |

**Modify:**

| Path | Change |
|---|---|
| `plugin/cmd/token-bay-sidecar/main.go:22` | Register the new `run` subcommand alongside `version` |

---

## Task 1: Package skeleton — errors, doc, public type

**Files:**
- Create: `plugin/internal/sidecar/doc.go`
- Create: `plugin/internal/sidecar/errors.go`
- Create: `plugin/internal/sidecar/errors_test.go`

- [ ] **Step 1: Write the failing test**

```go
// plugin/internal/sidecar/errors_test.go
package sidecar

import (
	"errors"
	"testing"
)

func TestSentinelsAreDistinct(t *testing.T) {
	if errors.Is(ErrInvalidDeps, ErrAlreadyStarted) {
		t.Fatal("ErrInvalidDeps and ErrAlreadyStarted must be distinct")
	}
	if errors.Is(ErrAlreadyStarted, ErrClosed) {
		t.Fatal("ErrAlreadyStarted and ErrClosed must be distinct")
	}
	if errors.Is(ErrInvalidDeps, ErrClosed) {
		t.Fatal("ErrInvalidDeps and ErrClosed must be distinct")
	}
}

func TestSentinelsHaveStableMessages(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{ErrInvalidDeps, "sidecar: invalid deps"},
		{ErrAlreadyStarted, "sidecar: already started"},
		{ErrClosed, "sidecar: closed"},
	}
	for _, tc := range cases {
		if tc.err.Error() != tc.want {
			t.Errorf("got %q, want %q", tc.err.Error(), tc.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./plugin/internal/sidecar/... -run TestSentinels`
Expected: build error — `undefined: ErrInvalidDeps` (etc.).

- [ ] **Step 3: Write minimal implementation**

```go
// plugin/internal/sidecar/doc.go
// Package sidecar is the long-lived supervisor for the token-bay-sidecar
// process. It composes the plugin's subsystems (identity, auditlog,
// trackerclient, ccproxy) into a single App with a Run/Close lifecycle.
//
// See docs/superpowers/specs/plugin/2026-04-22-plugin-design.md §2.1, §3.
package sidecar
```

```go
// plugin/internal/sidecar/errors.go
package sidecar

import "errors"

// ErrInvalidDeps is returned by New when the supplied Deps fail Validate.
var ErrInvalidDeps = errors.New("sidecar: invalid deps")

// ErrAlreadyStarted is returned by Run on the second call.
var ErrAlreadyStarted = errors.New("sidecar: already started")

// ErrClosed is returned by Run after Close has been called.
var ErrClosed = errors.New("sidecar: closed")
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./plugin/internal/sidecar/... -run TestSentinels -v`
Expected: PASS for both `TestSentinelsAreDistinct` and `TestSentinelsHaveStableMessages`.

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/sidecar/doc.go plugin/internal/sidecar/errors.go plugin/internal/sidecar/errors_test.go
git commit -m "feat(plugin/sidecar): package skeleton + error sentinels"
```

---

## Task 2: Deps struct + Validate

**Files:**
- Create: `plugin/internal/sidecar/deps.go`
- Create: `plugin/internal/sidecar/deps_test.go`

The `Deps` struct names every input the supervisor needs. Each field is the *result* of work done by the cmd layer (config loading, identity opening, audit log opening, etc.) — the supervisor doesn't touch the filesystem itself. This keeps `New` deterministic and trivially testable.

- [ ] **Step 1: Write the failing test**

```go
// plugin/internal/sidecar/deps_test.go
package sidecar

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/auditlog"
	"github.com/token-bay/token-bay/plugin/internal/identity"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
)

func validDeps(t *testing.T) Deps {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := identity.New(priv)
	require.NoError(t, err)

	dir := t.TempDir()
	logger, err := auditlog.Open(filepath.Join(dir, "audit.log"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = logger.Close() })

	return Deps{
		Logger:    zerolog.Nop(),
		Signer:    signer,
		AuditLog:  logger,
		CCProxyAddr: "127.0.0.1:0",
		TrackerEndpoints: []trackerclient.TrackerEndpoint{
			{Addr: "tracker.example:7443", IdentityHash: [32]byte{1}, Region: "test"},
		},
	}
}

func TestDeps_Validate_Happy(t *testing.T) {
	deps := validDeps(t)
	require.NoError(t, deps.Validate())
}

func TestDeps_Validate_NilSigner(t *testing.T) {
	deps := validDeps(t)
	deps.Signer = nil
	err := deps.Validate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidDeps))
	require.Contains(t, err.Error(), "Signer")
}

func TestDeps_Validate_NilAuditLog(t *testing.T) {
	deps := validDeps(t)
	deps.AuditLog = nil
	err := deps.Validate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidDeps))
	require.Contains(t, err.Error(), "AuditLog")
}

func TestDeps_Validate_EmptyCCProxyAddr(t *testing.T) {
	deps := validDeps(t)
	deps.CCProxyAddr = ""
	err := deps.Validate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidDeps))
	require.Contains(t, err.Error(), "CCProxyAddr")
}

func TestDeps_Validate_EmptyTrackerEndpoints(t *testing.T) {
	deps := validDeps(t)
	deps.TrackerEndpoints = nil
	err := deps.Validate()
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidDeps))
	require.Contains(t, err.Error(), "TrackerEndpoints")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./plugin/internal/sidecar/... -run TestDeps_ -v`
Expected: build error — `undefined: Deps`.

- [ ] **Step 3: Write minimal implementation**

```go
// plugin/internal/sidecar/deps.go
package sidecar

import (
	"fmt"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/plugin/internal/auditlog"
	"github.com/token-bay/token-bay/plugin/internal/identity"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
)

// Deps is everything the supervisor needs to construct itself. The cmd
// layer (cmd/token-bay-sidecar/run_cmd.go) is responsible for building
// each field from disk-resident inputs (config, identity files, audit log
// path, bootstrap endpoints).
//
// Optional fields are documented inline. Required fields must satisfy
// Validate.
type Deps struct {
	// Logger is the structured logger used by the supervisor and passed
	// down to subsystems that accept one. Required.
	Logger zerolog.Logger

	// Signer is the plugin's Ed25519 keypair holder, satisfying the
	// trackerclient.Signer interface. Built from internal/identity.Open.
	// Required.
	Signer *identity.Signer

	// AuditLog is the open append-only writer for ~/.token-bay/audit.log.
	// The supervisor closes it at shutdown. Required.
	AuditLog *auditlog.Logger

	// CCProxyAddr is the bind address for the local Anthropic-compatible
	// HTTP server (plugin spec §2.5). Use "127.0.0.1:0" to let the OS
	// pick a port — the resolved address is exposed via Status. Required.
	CCProxyAddr string

	// TrackerEndpoints is the bootstrap list handed to the trackerclient.
	// Empty list is rejected. Required.
	TrackerEndpoints []trackerclient.TrackerEndpoint

	// TrackerClientOverrides lets the cmd layer tweak trackerclient.Config
	// fields (timeouts, backoff, etc.) before validation. Optional.
	TrackerClientOverrides func(*trackerclient.Config)
}

// Validate enforces required-field invariants. Returns an ErrInvalidDeps
// chain on failure.
func (d Deps) Validate() error {
	if d.Signer == nil {
		return fmt.Errorf("%w: Signer must be non-nil", ErrInvalidDeps)
	}
	if d.AuditLog == nil {
		return fmt.Errorf("%w: AuditLog must be non-nil", ErrInvalidDeps)
	}
	if d.CCProxyAddr == "" {
		return fmt.Errorf("%w: CCProxyAddr must be non-empty", ErrInvalidDeps)
	}
	if len(d.TrackerEndpoints) == 0 {
		return fmt.Errorf("%w: TrackerEndpoints must be non-empty", ErrInvalidDeps)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./plugin/internal/sidecar/... -run TestDeps_ -v`
Expected: PASS for all five `TestDeps_*` cases.

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/sidecar/deps.go plugin/internal/sidecar/deps_test.go
git commit -m "feat(plugin/sidecar): Deps struct with Validate"
```

---

## Task 3: App.New constructs subsystems

**Files:**
- Create: `plugin/internal/sidecar/sidecar.go`
- Create: `plugin/internal/sidecar/sidecar_test.go`

`New` builds the trackerclient and ccproxy from the deps. It does **not** start them — `Run` does that. This split lets tests inspect the assembled `App` (e.g. verify the trackerclient signer matches the deps signer) without spinning up goroutines.

- [ ] **Step 1: Write the failing test**

```go
// plugin/internal/sidecar/sidecar_test.go
package sidecar

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNew_HappyPath(t *testing.T) {
	deps := validDeps(t)
	app, err := New(deps)
	require.NoError(t, err)
	require.NotNil(t, app)
	require.NotNil(t, app.tracker)
	require.NotNil(t, app.proxy)
}

func TestNew_RejectsInvalidDeps(t *testing.T) {
	deps := validDeps(t)
	deps.Signer = nil
	app, err := New(deps)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidDeps))
	require.Nil(t, app)
}

func TestNew_AppliesTrackerClientOverrides(t *testing.T) {
	deps := validDeps(t)
	called := false
	deps.TrackerClientOverrides = func(_ *struct{}) {
		// placeholder; replaced by typed signature below
	}
	_ = called
	_ = deps
	// real assertion exercised in Step 3 once the type is wired.
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./plugin/internal/sidecar/... -run TestNew_ -v`
Expected: build error — `undefined: New`.

- [ ] **Step 3: Write minimal implementation**

```go
// plugin/internal/sidecar/sidecar.go
package sidecar

import (
	"context"
	"fmt"
	"sync"

	"github.com/token-bay/token-bay/plugin/internal/ccproxy"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
)

// App is the running supervisor. Construct via New; drive via Run; release
// via Close.
type App struct {
	deps Deps

	tracker *trackerclient.Client
	proxy   *ccproxy.Server

	startMu sync.Mutex
	started bool
	closed  bool
}

// New validates deps and constructs the trackerclient + ccproxy. The
// returned App has not started any goroutines.
func New(deps Deps) (*App, error) {
	if err := deps.Validate(); err != nil {
		return nil, err
	}

	tcCfg := trackerclient.Config{
		Endpoints: deps.TrackerEndpoints,
		Identity:  deps.Signer,
		Logger:    deps.Logger,
	}
	if deps.TrackerClientOverrides != nil {
		deps.TrackerClientOverrides(&tcCfg)
	}
	tracker, err := trackerclient.New(tcCfg)
	if err != nil {
		return nil, fmt.Errorf("sidecar: build trackerclient: %w", err)
	}

	proxy := ccproxy.New(ccproxy.WithAddr(deps.CCProxyAddr))

	return &App{
		deps:    deps,
		tracker: tracker,
		proxy:   proxy,
	}, nil
}

// Run starts every subsystem and blocks until ctx is cancelled or any
// subsystem fails. Returns the first error observed during startup or
// shutdown; returns nil on a clean ctx-cancel.
func (a *App) Run(ctx context.Context) error {
	_ = ctx // implemented in Task 4
	return nil
}

// Close stops the supervisor. Idempotent.
func (a *App) Close() error {
	a.startMu.Lock()
	defer a.startMu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	return nil
}
```

Replace the placeholder `TestNew_AppliesTrackerClientOverrides` with the real assertion:

```go
// plugin/internal/sidecar/sidecar_test.go (replace the placeholder test)
func TestNew_AppliesTrackerClientOverrides(t *testing.T) {
	deps := validDeps(t)
	called := false
	deps.TrackerClientOverrides = func(cfg *trackerclient.Config) {
		called = true
		cfg.MaxFrameSize = 4096
	}
	_, err := New(deps)
	require.NoError(t, err)
	require.True(t, called, "override callback must be invoked")
}
```

(Imports for `trackerclient` are already present via `validDeps`.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./plugin/internal/sidecar/... -run TestNew_ -v`
Expected: PASS for `TestNew_HappyPath`, `TestNew_RejectsInvalidDeps`, `TestNew_AppliesTrackerClientOverrides`.

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/sidecar/sidecar.go plugin/internal/sidecar/sidecar_test.go
git commit -m "feat(plugin/sidecar): New constructs trackerclient + ccproxy"
```

---

## Task 4: Run lifecycle — errgroup + ordered shutdown

**Files:**
- Modify: `plugin/internal/sidecar/sidecar.go` (extend `Run` and `Close`)
- Modify: `plugin/internal/sidecar/sidecar_test.go` (add lifecycle tests)

`Run` starts ccproxy.Start and trackerclient.Start under an errgroup. ccproxy is the only subsystem that can fail synchronously here (port-bind error), so its error returns directly. The trackerclient supervisor runs until ctx-cancel; its `Start` returns nil on success and the supervisor goroutine handles its own errors via reconnect/backoff.

On shutdown: cancel ctx → `proxy.Shutdown(graceCtx)` → `tracker.Close()` → `auditlog.Close()`. Each step is best-effort; errors are joined and returned.

- [ ] **Step 1: Write the failing test**

```go
// plugin/internal/sidecar/sidecar_test.go (append)

func TestRun_ReturnsOnCtxCancel(t *testing.T) {
	deps := validDeps(t)
	app, err := New(deps)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- app.Run(ctx) }()

	// Give Run time to start subsystems.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of ctx cancel")
	}
}

func TestRun_RejectsSecondCall(t *testing.T) {
	deps := validDeps(t)
	app, err := New(deps)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = app.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	err = app.Run(ctx)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrAlreadyStarted))
}

func TestRun_RejectsAfterClose(t *testing.T) {
	deps := validDeps(t)
	app, err := New(deps)
	require.NoError(t, err)

	require.NoError(t, app.Close())

	err = app.Run(context.Background())
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrClosed))
}

func TestRun_CCProxyBindErrorPropagates(t *testing.T) {
	deps := validDeps(t)
	deps.CCProxyAddr = "127.0.0.1:1" // privileged port — bind will fail unbiased
	app, err := New(deps)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err = app.Run(ctx)
	require.Error(t, err, "Run must return the bind error")
}
```

(Add `import "context"` and `import "time"` at the top of the test file if not present.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./plugin/internal/sidecar/... -run TestRun_ -v`
Expected: FAIL — `Run` is the no-op stub from Task 3, so cancel never returns / second-call never errors / etc.

- [ ] **Step 3: Replace `Run` and `Close` with the real implementation**

```go
// plugin/internal/sidecar/sidecar.go (replace Run and Close)

import (
	// add to existing imports:
	"errors"
	"time"

	"golang.org/x/sync/errgroup"
)

// Run starts every subsystem and blocks until ctx is cancelled or any
// subsystem fails. Subsystem startup is synchronous (ccproxy must bind
// before Run returns). Shutdown order on exit: ccproxy → trackerclient →
// auditlog.
func (a *App) Run(ctx context.Context) error {
	a.startMu.Lock()
	if a.closed {
		a.startMu.Unlock()
		return ErrClosed
	}
	if a.started {
		a.startMu.Unlock()
		return ErrAlreadyStarted
	}
	a.started = true
	a.startMu.Unlock()

	// ccproxy: synchronous bind. If this fails, no goroutines were
	// started; return the error directly.
	if err := a.proxy.Start(ctx); err != nil {
		return fmt.Errorf("sidecar: start ccproxy: %w", err)
	}

	// trackerclient: starts its own supervisor goroutine. Start itself
	// returns synchronously; the supervisor lives until Close.
	if err := a.tracker.Start(ctx); err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = a.proxy.Shutdown(shutdownCtx)
		return fmt.Errorf("sidecar: start trackerclient: %w", err)
	}

	// Block until ctx-cancel. There is no foreground goroutine to wait
	// on — both subsystems own their own background loops driven by the
	// same ctx.
	<-ctx.Done()

	return a.shutdown()
}

func (a *App) shutdown() error {
	graceCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var errs []error
	if err := a.proxy.Shutdown(graceCtx); err != nil {
		errs = append(errs, fmt.Errorf("ccproxy shutdown: %w", err))
	}
	if err := a.tracker.Close(); err != nil {
		errs = append(errs, fmt.Errorf("trackerclient close: %w", err))
	}
	if err := a.deps.AuditLog.Close(); err != nil {
		errs = append(errs, fmt.Errorf("auditlog close: %w", err))
	}
	return errors.Join(errs...)
}

// Close stops the supervisor. Safe to call before or after Run; safe to
// call concurrently with Run as long as ctx is cancelled separately.
func (a *App) Close() error {
	a.startMu.Lock()
	if a.closed {
		a.startMu.Unlock()
		return nil
	}
	a.closed = true
	started := a.started
	a.startMu.Unlock()

	if !started {
		// Subsystems were never started; close auditlog only (the open
		// fd is the caller's responsibility otherwise).
		return a.deps.AuditLog.Close()
	}
	// If Run is in flight, the caller is responsible for cancelling its
	// ctx; shutdown is invoked from Run itself.
	return nil
}
```

The unused import `errgroup` from the imports stanza is a placeholder for the next task; remove it now if your linter rejects unused imports — `golangci-lint`'s `unused` linter will flag it.

> **Note:** the `errgroup` import is *not* needed by the implementation above. Strike it from the import block when adding the new ones (`errors`, `time`).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./plugin/internal/sidecar/... -run TestRun_ -v`
Expected: PASS for all four `TestRun_*` cases.

Run: `go test -race ./plugin/internal/sidecar/...`
Expected: PASS, no race warnings.

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/sidecar/sidecar.go plugin/internal/sidecar/sidecar_test.go
git commit -m "feat(plugin/sidecar): Run lifecycle with ordered shutdown"
```

---

## Task 5: Status snapshot

**Files:**
- Create: `plugin/internal/sidecar/status.go`
- Create: `plugin/internal/sidecar/status_test.go`

`/token-bay status` will eventually call `App.Status()` over IPC. For this plan we only build the in-process snapshot type and assemble it from the live subsystems. It carries the fields a slash-command renderer needs: tracker connection phase + endpoint, ccproxy bound address, sidecar version, started_at.

- [ ] **Step 1: Write the failing test**

```go
// plugin/internal/sidecar/status_test.go
package sidecar

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
)

func TestStatus_BeforeRun(t *testing.T) {
	deps := validDeps(t)
	app, err := New(deps)
	require.NoError(t, err)

	st := app.Status()
	require.False(t, st.Running)
	require.Equal(t, trackerclient.PhaseDisconnected, st.Tracker.Phase)
	require.Empty(t, st.CCProxyAddr, "no resolved bind address before Start")
}

func TestStatus_AfterStart_HasResolvedCCProxyAddr(t *testing.T) {
	deps := validDeps(t)
	app, err := New(deps)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = app.Run(ctx) }()

	require.Eventually(t, func() bool {
		return app.Status().Running && app.Status().CCProxyAddr != ""
	}, 2*time.Second, 20*time.Millisecond)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./plugin/internal/sidecar/... -run TestStatus_ -v`
Expected: build error — `undefined: app.Status`.

- [ ] **Step 3: Implement Status**

```go
// plugin/internal/sidecar/status.go
package sidecar

import (
	"time"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
)

// Status is a snapshot of supervisor + subsystem state, suitable for
// rendering by `/token-bay status`.
type Status struct {
	Running     bool
	StartedAt   time.Time
	CCProxyAddr string
	Tracker     trackerclient.ConnectionState
}

// Status returns a current snapshot. Safe to call concurrently with Run.
func (a *App) Status() Status {
	a.startMu.Lock()
	running := a.started && !a.closed
	startedAt := a.startedAt
	a.startMu.Unlock()

	return Status{
		Running:     running,
		StartedAt:   startedAt,
		CCProxyAddr: a.proxy.ResolvedAddr(),
		Tracker:     a.tracker.Status(),
	}
}
```

Add `startedAt` to the `App` struct in `sidecar.go` and stamp it inside `Run` immediately after marking `started = true`:

```go
// plugin/internal/sidecar/sidecar.go (App struct — add field)
type App struct {
	deps Deps

	tracker *trackerclient.Client
	proxy   *ccproxy.Server

	startMu   sync.Mutex
	started   bool
	closed    bool
	startedAt time.Time
}
```

```go
// plugin/internal/sidecar/sidecar.go (inside Run, after a.started = true)
a.startedAt = time.Now()
```

`ResolvedAddr` is the existing public accessor on `ccproxy.Server`. Verify by grepping:

```bash
grep -n "func (s \*Server) ResolvedAddr" plugin/internal/ccproxy/*.go
```

If the symbol is named differently, adapt the `Status` accessor to match — read the ccproxy file before changing the name in this plan; the call site is the only place that depends on the spelling.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./plugin/internal/sidecar/... -run TestStatus_ -v`
Expected: PASS for both `TestStatus_*`.

- [ ] **Step 5: Commit**

```bash
git add plugin/internal/sidecar/status.go plugin/internal/sidecar/status_test.go plugin/internal/sidecar/sidecar.go
git commit -m "feat(plugin/sidecar): Status snapshot for slash commands"
```

---

## Task 6: cmd/token-bay-sidecar — `run` subcommand

**Files:**
- Create: `plugin/cmd/token-bay-sidecar/run_cmd.go`
- Create: `plugin/cmd/token-bay-sidecar/run_cmd_test.go`
- Modify: `plugin/cmd/token-bay-sidecar/main.go:22` — register the `run` subcommand

The cmd layer turns disk-resident inputs into `sidecar.Deps`:
- `--config <path>` → `internal/config.Load`
- Identity: open `<config-dir>/identity.key` + `<config-dir>/identity.json` via `internal/identity.Open`. If absent, return a clear "run /token-bay enroll first" error.
- Audit log: `internal/auditlog.Open(cfg.AuditLogPath)`.
- Tracker endpoints: parse `cfg.Tracker`. `auto` is rejected (no resolver yet). A URL form requires an `IdentityHash` from env `TOKEN_BAY_TRACKER_HASH` (hex) — if missing, error.
- ccproxy address: `127.0.0.1:0` for v1 (OS-assigned). Address is logged and returned via Status.

- [ ] **Step 1: Write the failing test**

```go
// plugin/cmd/token-bay-sidecar/run_cmd_test.go
package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRunCmd_RequiresConfigFlag(t *testing.T) {
	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run"})

	err := cmd.Execute()
	require.Error(t, err)
	require.Contains(t, buf.String()+err.Error(), "config")
}

func TestRunCmd_RejectsTrackerAuto(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeMinimalConfig(t, dir, "auto")

	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run", "--config", cfgPath})

	err := cmd.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "auto")
}

// writeMinimalConfig writes the smallest config.yaml that passes
// internal/config.Validate, plus a freshly-generated identity in the
// config-adjacent dir.
func writeMinimalConfig(t *testing.T, dir, tracker string) string {
	t.Helper()
	// Implementation detail: this helper is shared with TestRunCmd_StartsAndStops below.
	// Create identity files via internal/identity.Generate + SaveKey/SaveRecord,
	// audit log path under dir, settings.json under dir, env var
	// TOKEN_BAY_TRACKER_HASH set to hex of [32]byte{1}.
	t.Setenv("TOKEN_BAY_TRACKER_HASH", "0100000000000000000000000000000000000000000000000000000000000000")
	// ... write yaml with role=both, tracker=tracker, paths under dir
	// (full body shown in implementation step)
	panic("write helper: see Step 3")
}

func TestRunCmd_StartsAndStops(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeMinimalConfig(t, dir, "https://tracker.example:7443")

	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run", "--config", cfgPath})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd.SetContext(ctx)

	errCh := make(chan error, 1)
	go func() { errCh <- cmd.Execute() }()

	// Cancel after a brief delay to let startup complete.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		// ctx-cancel returns nil from sidecar.Run; cmd.Execute may wrap
		// or pass through unchanged.
		_ = err
	case <-time.After(3 * time.Second):
		t.Fatal("run did not return within 3s of ctx cancel")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./plugin/cmd/token-bay-sidecar/... -v`
Expected: build error — `undefined: newRunCmd` (when registered) or panic from helper.

- [ ] **Step 3: Implement the run subcommand and helper**

```go
// plugin/cmd/token-bay-sidecar/run_cmd.go
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/token-bay/token-bay/plugin/internal/auditlog"
	"github.com/token-bay/token-bay/plugin/internal/config"
	"github.com/token-bay/token-bay/plugin/internal/identity"
	"github.com/token-bay/token-bay/plugin/internal/sidecar"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
)

const (
	defaultCCProxyAddr = "127.0.0.1:0"
	envTrackerHash     = "TOKEN_BAY_TRACKER_HASH" //nolint:gosec // env var name, not a credential
)

func newRunCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Start the token-bay sidecar",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if configPath == "" {
				return errors.New("--config is required")
			}
			cfg, err := config.Load(configPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			cfgDir := filepath.Dir(configPath)
			signer, _, err := identity.Open(cfgDir)
			if err != nil {
				return fmt.Errorf("open identity at %s (run /token-bay enroll first?): %w", cfgDir, err)
			}

			al, err := auditlog.Open(cfg.AuditLogPath)
			if err != nil {
				return fmt.Errorf("open audit log: %w", err)
			}

			endpoints, err := resolveTrackerEndpoints(cfg.Tracker)
			if err != nil {
				_ = al.Close()
				return err
			}

			deps := sidecar.Deps{
				Logger:           zerolog.New(cmd.ErrOrStderr()).With().Timestamp().Logger(),
				Signer:           signer,
				AuditLog:         al,
				CCProxyAddr:      defaultCCProxyAddr,
				TrackerEndpoints: endpoints,
			}

			app, err := sidecar.New(deps)
			if err != nil {
				_ = al.Close()
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return app.Run(ctx)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to ~/.token-bay/config.yaml (required)")
	return cmd
}

// resolveTrackerEndpoints turns cfg.Tracker into a one-element bootstrap
// list. v1 only supports an explicit URL (host:port form via the URL's
// Host) — `auto` is rejected because no resolver exists yet.
func resolveTrackerEndpoints(spec string) ([]trackerclient.TrackerEndpoint, error) {
	if spec == "auto" {
		return nil, errors.New("tracker: auto-bootstrap not yet implemented; configure an explicit tracker URL")
	}
	u, err := url.Parse(spec)
	if err != nil {
		return nil, fmt.Errorf("parse tracker URL %q: %w", spec, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("tracker URL %q has no host", spec)
	}

	hashHex := strings.TrimSpace(os.Getenv(envTrackerHash))
	if hashHex == "" {
		return nil, fmt.Errorf("%s env var must be set to the hex SHA-256 of the tracker's Ed25519 SPKI", envTrackerHash)
	}
	raw, err := hex.DecodeString(hashHex)
	if err != nil || len(raw) != sha256.Size {
		return nil, fmt.Errorf("%s must be %d hex bytes", envTrackerHash, sha256.Size)
	}
	var hash [32]byte
	copy(hash[:], raw)

	return []trackerclient.TrackerEndpoint{{
		Addr:         u.Host,
		IdentityHash: hash,
		Region:       "configured",
	}}, nil
}
```

Replace the `panic("write helper: see Step 3")` placeholder with the real helper:

```go
// plugin/cmd/token-bay-sidecar/run_cmd_test.go (replace placeholder body)
import (
	// add to existing imports:
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"

	"github.com/token-bay/token-bay/plugin/internal/identity"
	"github.com/token-bay/token-bay/shared/ids"
)

func writeMinimalConfig(t *testing.T, dir, tracker string) string {
	t.Helper()
	t.Setenv("TOKEN_BAY_TRACKER_HASH", "0100000000000000000000000000000000000000000000000000000000000000")

	// Identity files.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	require.NoError(t, identity.SaveKey(filepath.Join(dir, "identity.key"), priv))
	rec := &identity.Record{
		Version:    1,
		IdentityID: ids.IdentityID{},
		Role:       1,
	}
	require.NoError(t, identity.SaveRecord(filepath.Join(dir, "identity.json"), rec))

	// config.yaml.
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "role: both\n" +
		"tracker: " + tracker + "\n" +
		"audit_log_path: " + filepath.Join(dir, "audit.log") + "\n"
	require.NoError(t, os.WriteFile(cfgPath, []byte(body), 0o600))
	return cfgPath
}
```

(`identity.SaveKey` / `identity.SaveRecord` exist in `internal/identity`; if the names differ, run `grep -n "func Save" plugin/internal/identity/*.go` and adjust this helper to match the current API.)

Wire the new subcommand from `main.go`:

```go
// plugin/cmd/token-bay-sidecar/main.go (modify newRootCmd; line 22 area)
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "token-bay-sidecar",
		Short: "Token-Bay plugin sidecar",
	}
	root.AddCommand(newVersionCmd())
	root.AddCommand(newRunCmd())
	return root
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./plugin/cmd/token-bay-sidecar/... -v`
Expected: PASS for `TestRootCmd_Version_PrintsExpected`, `TestRunCmd_RequiresConfigFlag`, `TestRunCmd_RejectsTrackerAuto`, `TestRunCmd_StartsAndStops`.

- [ ] **Step 5: Commit**

```bash
git add plugin/cmd/token-bay-sidecar/main.go plugin/cmd/token-bay-sidecar/run_cmd.go plugin/cmd/token-bay-sidecar/run_cmd_test.go
git commit -m "feat(plugin/sidecar): cmd run subcommand wires config + identity + endpoints"
```

---

## Task 7: Module CLAUDE.md

**Files:**
- Create: `plugin/internal/sidecar/CLAUDE.md`

The repo convention is "every internal module has a CLAUDE.md" (see `internal/auditlog/CLAUDE.md`, `internal/config/CLAUDE.md`, `internal/identity/CLAUDE.md`). Mirror their tone: purpose, public API table, non-negotiable rules, source-file map.

- [ ] **Step 1: Write the file**

```markdown
# plugin/internal/sidecar — Development Context

## What this is

The long-lived supervisor for the `token-bay-sidecar` process. Composes the plugin's
subsystems (identity, auditlog, trackerclient, ccproxy) into one `App` with a
Run/Close lifecycle. No business logic — every concrete behavior (slash-command
handlers, hook ingestion, network-mode coordination, offer/settlement push handlers)
lives in its own package and is wired in via a follow-on plan.

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

1. **No filesystem I/O.** All disk-resident state (config file, identity files, audit
   log, settings.json) is opened by the cmd layer and handed in via `Deps`. The
   supervisor must remain trivially testable without `t.TempDir()` plumbing.
2. **No business logic.** Network-mode coordination, offer/settlement handling, and
   slash-command routing each have their own package. Adding a method here that does
   anything other than start/stop/observe is a layering smell.
3. **Subsystem startup is synchronous.** A bind error in `ccproxy.Start` must
   surface from `Run` *before* it blocks on ctx. Background loops own their own
   reconnect; a mid-run subsystem failure does not abort the supervisor.
4. **Shutdown order is ccproxy → trackerclient → auditlog.** ccproxy first so no
   inbound traffic; tracker next so push acceptors drain; auditlog last because
   shutdown writes its own final entry (when the future hook owner adds one).
5. **`Run` is single-shot.** `ErrAlreadyStarted` on the second call. Restart = new
   `App`.

## Things that look surprising and aren't bugs

- `Close` *before* `Run` releases the audit log handle (the only caller-owned
  fd not transferred via `Run`'s shutdown path). After `Run` it's a no-op
  because shutdown already ran.
- `TrackerClientOverrides` is a callback, not a Config copy. Lets the cmd
  layer flip a single timeout in tests without re-declaring the whole
  trackerclient.Config.
- `Deps.AuditLog` is opened by the caller because audit-log paths come from
  config and the caller already touched config; threading it through the
  supervisor would just relay the same value.
- `OfferHandler` and `SettlementHandler` are not in `Deps` yet — trackerclient
  tolerates nil for both, and the seeder/consumer push paths land in their own
  feature plans.
```

- [ ] **Step 2: Commit**

```bash
git add plugin/internal/sidecar/CLAUDE.md
git commit -m "docs(plugin/sidecar): module CLAUDE.md"
```

---

## Task 8: Repo-wide green check

**Files:** none modified — verification only.

- [ ] **Step 1: Run the test suite**

Run: `make -C plugin test`
Expected: all packages PASS, race detector clean.

- [ ] **Step 2: Run the linter**

Run: `make -C plugin lint`
Expected: zero issues.

- [ ] **Step 3: Build the binary**

Run: `make -C plugin build`
Expected: produces `plugin/bin/token-bay-sidecar`.

- [ ] **Step 4: Smoke-test the binary**

Run:
```bash
./plugin/bin/token-bay-sidecar version
./plugin/bin/token-bay-sidecar run 2>&1 || true
```
Expected: first prints version line; second errors with "--config is required".

- [ ] **Step 5: No commit needed**

Verification only. If any step fails, fix in place and re-run before opening the PR.

---

## Self-review checklist

- [x] **Spec coverage:** Plugin spec §2.1 says the sidecar is "the long-running background service that maintains the long-lived tracker connection, handles incoming seeder offers, runs the availability state machine." This plan covers tracker connection lifecycle (Tasks 3, 4) and ccproxy bring-up (Tasks 3, 4); it explicitly defers offer handling and availability state machine to follow-on plans, and the deferrals are listed in the *Out of scope* section so a future implementer doesn't think they're missing.
- [x] **Placeholder scan:** every code step shows complete code; the `writeMinimalConfig` helper is fully written in Step 3 of Task 6.
- [x] **Type consistency:** `Deps` field names (`Signer`, `AuditLog`, `CCProxyAddr`, `TrackerEndpoints`, `TrackerClientOverrides`, `Logger`) are stable across Tasks 2-6. `App.Status` returns `Status`; `Status.Tracker` is `trackerclient.ConnectionState` (the existing public type, verified above).
- [x] **API alignment:** `auditlog.Open`, `identity.Open`, `identity.New`, `identity.SaveKey`, `identity.SaveRecord`, `trackerclient.New/Start/Close/Status`, `ccproxy.New/Start/Shutdown` — every external call matches the existing public surface confirmed by reading the corresponding source files. The `ResolvedAddr` accessor on `ccproxy.Server` is the one place an implementer must verify (step in Task 5 instructs them to grep before assuming the spelling).

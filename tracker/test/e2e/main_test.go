//go:build e2e

// Package e2e_test drives the Docker Compose e2e topology
// (tracker/test/e2e/compose.e2e.yaml, plan Task 22) end-to-end via the
// driver package (tracker/test/e2e/driver, plan Task 23). TestMain owns
// the full stack lifecycle — generate → up → wait-healthy → run
// scenarios → always tear down — so every scenario file (this one plus
// the ones plan Tasks 25-29 add) just consumes the package-level
// accessors below and asserts against a stack that is already live.
package e2e_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/token-bay/token-bay/tracker/test/e2e/driver"
)

// Fixed, deterministic 32-byte Ed25519 seeds (64 hex chars each) fed to
// e2egen. Every key, tracker ID and SPKI pin the generator derives is a
// pure function of these seeds (see cmd/e2egen/main.go's doc comment) —
// pinning them here keeps the generated topology byte-identical across
// runs and avoids crypto/rand snowflakes in CI.
const (
	seedAHex   = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	seedBHex   = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	seedFedHex = "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
)

// Admin bearer tokens, matching compose.e2e.yaml's TOKEN_BAY_ADMIN_TOKEN
// env vars for tracker-a/-b.
const (
	adminATokenE2E = "e2e-admin-token-a"
	adminBTokenE2E = "e2e-admin-token-b"
)

// Host-side port mappings from compose.e2e.yaml.
const (
	adminABaseURL    = "http://localhost:9090"
	adminBBaseURL    = "http://localhost:9091"
	consumerBaseURL  = "http://localhost:8081"
	seederBaseURL    = "http://localhost:8082"
	fedactorBaseURL  = "http://localhost:8083"
	metricsABaseURL  = "http://localhost:9100"
	composeProjectID = "tokenbay-e2e"
)

// e2eDir is the absolute path to this package's directory
// (tracker/test/e2e), resolved from the source file location rather
// than the process's working directory. `go test` runs the compiled
// test binary with its cwd set to the package's source directory, which
// is NOT necessarily the "tracker/" repo-relative path a human would
// invoke `go test ./test/e2e/...` from — resolving via runtime.Caller
// keeps every path below correct regardless of invocation directory.
var e2eDir string

func init() {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("e2e: runtime.Caller(0) failed resolving package directory")
	}
	e2eDir = filepath.Dir(thisFile)
}

// stackDriver is the per-service container-control surface the scenarios use
// through compose(). Both the legacy docker-compose shell-out (driver.Compose,
// used by the E2E_REUSE_STACK coverage path) and the testcontainers-go
// topology (driver.Stack, the default path) satisfy it, so scenario code is
// identical on either backend.
type stackDriver interface {
	Exec(service string, args ...string) (string, error)
	Logs(service string) (string, error)
	Restart(service string) error
	Stop(service string) error
	Start(service string) error
	Kill(service, signal string) error
	PsAll() (string, error)
	Run(service, entrypoint string, args ...string) (string, error)
}

var (
	composeHandle stackDriver   // scenario container-control (Compose or Stack)
	stackHandle   *driver.Stack // non-nil on the default testcontainers path
	adminAClient  *driver.Admin
	adminBClient  *driver.Admin
	consumerCli   *driver.ConsumerCtl
	seederCli     *driver.SeederCtl
	fedactorCli   *driver.FedactorCtl
)

// adminA returns the driver.Admin client for tracker-a (host port 9090).
func adminA() *driver.Admin { return adminAClient }

// adminB returns the driver.Admin client for tracker-b (host port 9091).
func adminB() *driver.Admin { return adminBClient }

// consumerCtl returns the driver.ConsumerCtl client for the consumer
// actor (host port 8081).
func consumerCtl() *driver.ConsumerCtl { return consumerCli }

// seederCtl returns the driver.SeederCtl client for the seeder actor
// (host port 8082).
func seederCtl() *driver.SeederCtl { return seederCli }

// fedactorCtl returns the driver.FedactorCtl client for the Byzantine
// federation-neighbor actor (host port 8083). Not consumed by scenarios
// 1-2; kept here (this file establishes the shared accessors) for the
// federation scenario file plan Task 27 adds.
//
//nolint:unused // consumed once federation_test.go (plan Task 27) lands.
func fedactorCtl() *driver.FedactorCtl { return fedactorCli }

// compose returns the container-control handle for the live stack, for
// scenario files that need Exec/Logs/Restart/etc. Backed by driver.Stack
// (testcontainers) by default, or driver.Compose under E2E_REUSE_STACK.
func compose() stackDriver { return composeHandle }

// stack returns the testcontainers Stack handle (nil under E2E_REUSE_STACK),
// for scenarios that dynamically add containers (the concurrent multi-seeder
// matrix). Scenarios that need it must skip when it is nil.
func stack() *driver.Stack { return stackHandle }

// TestMain owns the whole-stack lifecycle for every e2e scenario in this
// package: generate the deterministic key/config artifacts, bring the
// compose topology up, poll every service until it reports healthy, run
// the scenario tests, and — always, even on a panic during bring-up or
// m.Run() — tear the stack down and remove the generated artifacts.
func TestMain(m *testing.M) {
	os.Exit(runTestMain(m))
}

func runTestMain(m *testing.M) (exitCode int) {
	genDir := filepath.Join(e2eDir, ".gen")

	// E2E_REUSE_STACK=1 makes TestMain attach to an already-running
	// compose stack instead of owning its lifecycle: skip artifact
	// generation, Up and teardown, but still poll every service healthy
	// before m.Run(). The coverage runner (test/e2e/run-cover.sh) uses
	// this — it must start/stop the stack itself with an extra compose
	// override (-f compose.cover.yaml) and SIGTERM the trackers after
	// the tests so the coverage runtime flushes.
	reuseStack := os.Getenv("E2E_REUSE_STACK") == "1"

	composeFile := filepath.Join(e2eDir, "compose.e2e.yaml")
	if reuseStack {
		// Coverage path: attach to a stack the caller (run-cover.sh) brought
		// up itself with compose overrides, via the legacy shell-out driver.
		cmp := driver.Compose{File: composeFile, Project: composeProjectID}
		// E2E_COMPOSE_EXTRA_FILES (os.PathListSeparator-separated) layers
		// override files onto every compose invocation the scenarios make
		// through compose() — required alongside E2E_REUSE_STACK so
		// subcommands that materialize new containers from the file
		// definitions match the running stack's overridden topology.
		if extra := os.Getenv("E2E_COMPOSE_EXTRA_FILES"); extra != "" {
			cmp.ExtraFiles = filepath.SplitList(extra)
		}
		composeHandle = cmp
	}
	adminAClient = driver.NewAdmin(adminABaseURL, adminATokenE2E)
	adminBClient = driver.NewAdmin(adminBBaseURL, adminBTokenE2E)
	consumerCli = driver.NewConsumerCtl(consumerBaseURL)
	seederCli = driver.NewSeederCtl(seederBaseURL)
	fedactorCli = driver.NewFedactorCtl(fedactorBaseURL)

	// defer, not a plain call at the end of the function: this must run
	// even if generateArtifacts/composeHandle.Up/waitReady panics, or if
	// m.Run() panics (e.g. a scenario helper's require.* inside a
	// non-test goroutine). recover() here converts that into a failing
	// exit code instead of leaking a running compose stack.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintln(os.Stderr, "e2e: TestMain panic:", r)
			exitCode = 1
		}
		if !reuseStack {
			teardown(genDir)
		}
	}()

	if reuseStack {
		fmt.Fprintln(os.Stderr, "e2e: E2E_REUSE_STACK=1 — attaching to an already-running compose stack (no generate/up/teardown here; the caller owns the lifecycle)...")
	} else {
		if err := generateArtifacts(genDir); err != nil {
			fmt.Fprintln(os.Stderr, "e2e: generate artifacts:", err)
			return 1
		}

		fmt.Fprintln(os.Stderr, "e2e: bringing up the testcontainers stack (assumes token-bay-tracker:dev and tokenbay-e2e-actors:dev images already built — see make -C tracker test-e2e)...")
		st, err := driver.NewStack([]string{composeFile}, composeProjectID, genDir, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "e2e: new stack:", err)
			return 1
		}
		stackHandle = st
		composeHandle = st

		upCtx, upCancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer upCancel()
		if err := st.Up(upCtx); err != nil {
			fmt.Fprintln(os.Stderr, "e2e: stack up:", err)
			return 1
		}
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := waitReady(waitCtx); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: stack did not become ready:", err)
		if logs, logErr := composeHandle.PsAll(); logErr == nil {
			fmt.Fprintln(os.Stderr, "e2e: docker ps -a:\n"+logs)
		}
		return 1
	}

	return m.Run()
}

// generateArtifacts wipes any stale genDir and re-runs e2egen (Task 21)
// via `go run`, matching the same invocation path the make target
// (Task 30) uses, so a byte-identical .gen/ tree is produced whether the
// suite is driven by `make` or directly by `go test`.
func generateArtifacts(genDir string) error {
	if err := os.RemoveAll(genDir); err != nil {
		return fmt.Errorf("clean stale .gen: %w", err)
	}
	e2egenPkg := filepath.Join(e2eDir, "cmd", "e2egen")
	//nolint:gosec // G204: fixed local paths + fixed seed constants, not
	// external/attacker input — this shells out to `go run` the same way
	// the driver package shells out to `docker`.
	cmd := exec.Command("go", "run", e2egenPkg,
		"--out", genDir,
		"--seed-a", seedAHex,
		"--seed-b", seedBHex,
		"--seed-fed", seedFedHex,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go run %s: %w", e2egenPkg, err)
	}
	return nil
}

// teardown always runs at the end of runTestMain (via defer), whether
// bring-up failed, m.Run() completed, or something panicked in between.
// It removes named volumes (removeVolumes=true) so the next Up starts
// from a clean ledger, and removes the generated key/config artifacts
// (they are seed-derived and gitignored — never committed).
func teardown(genDir string) {
	if stackHandle != nil {
		downCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		if err := stackHandle.Down(downCtx); err != nil {
			fmt.Fprintln(os.Stderr, "e2e: stack down:", err)
		}
	}
	if err := os.RemoveAll(genDir); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: remove .gen:", err)
	}
}

// waitReady polls every service's health/readiness endpoint until each
// reports success or ctx's deadline elapses. Condition-polling, not a
// fixed sleep: compose's own healthcheck already gates admin API
// readiness (compose.e2e.yaml), but `docker compose up -d` returns as
// soon as containers are *created* — it does not block on healthchecks
// — so TestMain must poll independently before handing off to m.Run().
func waitReady(ctx context.Context) error {
	checks := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"tracker-a admin /health", func(ctx context.Context) error { _, err := adminAClient.Health(ctx); return err }},
		{"tracker-b admin /health", func(ctx context.Context) error { _, err := adminBClient.Health(ctx); return err }},
		{"consumer ctrl /healthz", consumerCli.Healthz},
		{"seeder ctrl /healthz", seederCli.Healthz},
		// fedactor (tracker/test/e2e/cmd/fedactor, plan Task 19) has no
		// /healthz route at all — its control mux only serves
		// /handshake, /send/*, and /received (control.go's mux()). GET
		// /received (200 + a JSON array, empty before any envelope
		// arrives) is the cheapest real signal its control server is up
		// and its actor state is initialized.
		{"fedactor ctrl /received", func(ctx context.Context) error { _, err := fedactorCli.Received(ctx); return err }},
	}
	for _, c := range checks {
		if err := pollUntilReady(ctx, c.name, c.fn); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "e2e: %s is ready\n", c.name)
	}
	return nil
}

// pollUntilReady calls check repeatedly (each attempt bounded to 5s) on
// a 1s cadence until it succeeds or ctx is done.
func pollUntilReady(ctx context.Context, name string, check func(context.Context) error) error {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	var lastErr error
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		lastErr = check(attemptCtx)
		cancel()
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s: %w (last error: %v)", name, ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

// eventually polls check on the given interval until it returns true or
// timeout elapses, failing the test (via t.Fatalf) if it never does.
// Scenario files share this instead of hand-rolling retry loops for
// conditions that settle asynchronously against the live stack (peer
// handshake completion, starter-grant ledger writes, etc.) —
// condition-polling, not fixed sleeps.
func eventually(t *testing.T, timeout, interval time.Duration, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if check() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for: %s", timeout, what)
		}
		time.Sleep(interval)
	}
}

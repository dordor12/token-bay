//go:build e2e

// Scenarios 8-10 of the plan's suite (Task 26): ledger integrity and
// durability across restarts, an on-disk corruption tripwire, and a
// graceful drain. All three share the SAME long-running compose stack as
// scenarios 1-7 (TestMain brings it up once for the whole package — see
// main_test.go) so none of them may leave tracker-a in a state that
// would break a later scenario or a subsequent full-suite run.
//
// # Corruption-target note (scenario 9)
//
// The plan's Task 26 Step 2 suggests corrupting the chain via:
//
//	UPDATE entries SET cost_credits = cost_credits + 1
//	WHERE seq = (SELECT MAX(seq) FROM entries);
//
// Empirically (verified live against this exact stack before writing this
// file) that statement does NOT trip the startup integrity gate, for two
// independent reasons:
//
//  1. `entries.cost_credits` is a denormalized/indexed projection column.
//     Every read path that feeds the integrity gate (storage.EntryBySeq,
//     storage.EntriesSince — see internal/ledger/storage/lookup.go's doc
//     comment "the indexed columns are projections, never read here")
//     reconstructs the entry Body from the separate `canonical` BLOB
//     column instead. Mutating `cost_credits` alone is invisible to
//     AssertChainIntegrity; tracker-a restarts healthy and re-logs
//     "ledger integrity verified at startup" as if nothing happened.
//  2. Even a semantically-valid edit of `canonical` itself (i.e. one that
//     re-marshals to valid protobuf, just with a different field value —
//     what an attacker actually altering their own balance would do) is
//     UNDETECTED when applied to the TIP entry specifically.
//     AssertChainIntegrity (internal/ledger/audit.go) only checks
//     hash(entry[n-1]) == entry[n].prev_hash walking forward; the tip has
//     no successor entry whose prev_hash could reveal a mismatch, and
//     neither the startup gate nor the peer-reconnect gate (both call the
//     same function) independently re-verifies a stored per-entry hash or
//     tracker_sig against the current row contents. This was confirmed
//     live: a validly-remarshaled, tampered tip entry passes the startup
//     gate. This is a genuine gap in the local integrity gate as
//     currently implemented — see the report filed alongside this task
//     (.superpowers/sdd/task-26-report.md) for full reproduction details;
//     it is NOT weakened around here, just not exercised by this test.
//
// What IS reliably, correctly caught by the current implementation is
// corruption of a NON-tip entry (any entry that still has a successor):
// the successor's stored prev_hash then mismatches the freshly recomputed
// hash of the tampered predecessor, exactly per spec §8's acceptance
// criterion. Every scenario run always has >= 2 ledger entries (the
// consumer's and seeder's starter grants land during actor enrollment
// regardless of which other scenario files ran in this invocation — see
// scenario 2 in bringup_test.go), so seq=1 always has a seq=2 successor.
// Scenario 9 below corrupts seq=1's `canonical` bytes for exactly that
// reason.
package e2e_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ledgerDBPath is tracker-a's on-disk ledger SQLite file inside the
// container (test/e2e/cmd/e2egen/render.go sets
// cfg.Ledger.StoragePath = "/data/ledger.sqlite").
const ledgerDBPath = "/data/ledger.sqlite"

// pollUntilTrue polls check on the given interval until it returns true
// or timeout elapses, returning whether it ever succeeded. Unlike
// eventually (main_test.go), this never calls t.Fatalf — it's safe to
// call from a t.Cleanup func, which may run during an already-unwinding
// test goroutine (e.g. after an earlier require.NoError failed). Callers
// report failure themselves via t.Errorf.
func pollUntilTrue(timeout, interval time.Duration, check func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if check() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(interval)
	}
}

// TestScenario08_RestartIntegrity is scenario 8 (Task 26 Step 1): after a
// clean `docker compose restart tracker-a`, the startup integrity gate
// must run again (a NEW "ledger integrity verified at startup" log line)
// and the ledger's tip must be exactly what it was before the restart —
// the chain is durable (persisted in the tracker-a-data volume), not
// rebuilt from scratch.
//
// Only requires a non-empty chain (>= 2 entries), not scenario-specific
// usage entries, so it passes whether this file runs alongside scenarios
// 1-7 or in isolation via `-run TestScenario08...` (the 2 starter grants
// always land during actor enrollment — see the package doc comment).
func TestScenario08_RestartIntegrity(t *testing.T) {
	ctx := context.Background()

	var tipBefore uint64
	require.True(t, pollUntilTrue(20*time.Second, 1*time.Second, func() bool {
		s, err := adminA().Stats(ctx)
		if err != nil || s.Ledger.TipSeq == nil {
			return false
		}
		tipBefore = *s.Ledger.TipSeq
		return true
	}), "tracker-a ledger should have a non-nil tip before restart")
	require.GreaterOrEqual(t, tipBefore, uint64(2), "at least the 2 starter-grant entries should exist")

	logsBefore, err := compose().Logs("tracker-a")
	require.NoError(t, err, "docker compose logs tracker-a (before restart)")
	verifiedCountBefore := strings.Count(logsBefore, "ledger integrity verified at startup")
	require.GreaterOrEqual(t, verifiedCountBefore, 1, "tracker-a's original bring-up should already have logged the startup gate once")

	require.NoError(t, compose().Restart("tracker-a"), "docker compose restart tracker-a")

	// Restart drops tracker-a's admin listener briefly (the process exits
	// and re-execs); poll rather than assume the compose healthcheck's
	// own cadence has already caught up.
	require.True(t, pollUntilTrue(30*time.Second, 1*time.Second, func() bool {
		h, herr := adminA().Health(ctx)
		return herr == nil && h.Status == "ok"
	}), "tracker-a /health should report ok again after restart")

	require.True(t, pollUntilTrue(15*time.Second, 1*time.Second, func() bool {
		logs, logErr := compose().Logs("tracker-a")
		return logErr == nil && strings.Count(logs, "ledger integrity verified at startup") > verifiedCountBefore
	}), "tracker-a logs should show a NEW startup integrity line after the restart")

	var tipAfter uint64
	require.True(t, pollUntilTrue(15*time.Second, 1*time.Second, func() bool {
		s, err := adminA().Stats(ctx)
		if err != nil || s.Ledger.TipSeq == nil {
			return false
		}
		tipAfter = *s.Ledger.TipSeq
		return true
	}), "tracker-a ledger tip should be readable again after restart")

	assert.Equal(t, tipBefore, tipAfter, "ledger tip_seq must be unchanged across a clean restart — the chain is durable, not rebuilt")
}

// TestScenario09_CorruptionTripwire is scenario 9 (Task 26 Step 2,
// NEGATIVE test): a corrupted on-disk ledger chain must NOT be served —
// the startup integrity gate (cmd/token-bay-tracker/integrity_gate.go)
// has to catch it and refuse to start. See the package doc comment above
// for why this corrupts seq=1 (a non-tip entry) rather than the plan's
// literal MAX(seq) suggestion.
//
// Self-restoring: this test backs up tracker-a's ledger DB before
// corrupting it and restores that backup via t.Cleanup, which the Go
// testing package guarantees runs even if an assertion earlier in this
// test fails (t.FailNow/require.* unwind through registered cleanups).
// Scenarios 1-7 and this file's own scenario 10 share the one compose
// stack this test runs against, so tracker-a MUST be healthy again with
// its original (uncorrupted) chain before this test returns.
func TestScenario09_CorruptionTripwire(t *testing.T) {
	ctx := context.Background()

	require.True(t, pollUntilTrue(20*time.Second, 1*time.Second, func() bool {
		s, err := adminA().Stats(ctx)
		return err == nil && s.Ledger.TipSeq != nil && *s.Ledger.TipSeq >= 2
	}), "tracker-a ledger tip_seq should be >= 2 before corrupting (need a non-tip entry to corrupt)")

	// Register the restore BEFORE corrupting anything, so it always runs
	// — including if the backup step itself fails partway through.
	t.Cleanup(func() { restoreTrackerA(t, ctx) })

	// 1) Force a WAL checkpoint so the ledger DB file is a single,
	// self-consistent snapshot (no separate -wal/-shm to reconcile on
	// restore), then back it up — all while tracker-a is still up and
	// healthy.
	_, err := compose().Exec("tracker-a", "sqlite3", ledgerDBPath, "PRAGMA wal_checkpoint(TRUNCATE);")
	require.NoError(t, err, "checkpoint WAL before backup")
	_, err = compose().Exec("tracker-a", "cp", "-f", ledgerDBPath, ledgerDBPath+".bak")
	require.NoError(t, err, "backup ledger DB before corrupting")

	// 2) Corrupt seq=1's canonical bytes. seq=2 (a starter grant, always
	// present) stores prev_hash = hash(seq=1's ORIGINAL body); once seq=1
	// is tampered, AssertChainIntegrity recomputes a different hash for
	// it and the forward-link check against seq=2 fails.
	_, err = compose().Exec("tracker-a", "sqlite3", ledgerDBPath,
		"UPDATE entries SET canonical = randomblob(length(canonical)) WHERE seq = 1;")
	require.NoError(t, err, "corrupt seq=1 canonical bytes")

	// 3) The corruption only affects what's on disk — tracker-a's
	// already-running process never re-reads it until its NEXT startup.
	// Stop (graceful) then start (in place, same container) so it runs
	// through the real startup integrity gate against the now-corrupted
	// chain.
	require.NoError(t, compose().Stop("tracker-a"), "stop tracker-a before restarting onto the corrupted chain")
	require.NoError(t, compose().Start("tracker-a"), "start tracker-a against the corrupted chain")

	// 4) The gate must trip: logs show the failure and tracker-a must
	// never report healthy on this corrupted chain. This tracker fails
	// fast (observed <2s live), so 20s is generous headroom, not a
	// required wait.
	require.True(t, pollUntilTrue(20*time.Second, 1*time.Second, func() bool {
		logs, logErr := compose().Logs("tracker-a")
		return logErr == nil && strings.Contains(logs, "ledger integrity check failed")
	}), "tracker-a logs should show the startup integrity failure")

	_, healthErr := adminA().Health(ctx)
	assert.Error(t, healthErr, "tracker-a must not serve /health against a corrupted ledger")

	// The tracker must never run NORMALLY on a corrupted chain. How that
	// manifests at the container level depends on the backend's restart
	// policy: docker-compose (restart:no) leaves it "Exited", while the
	// testcontainers backend re-launches it into the same failing gate
	// (crash-loop, seen mid-restart as "health: starting"). Either way it
	// must never reach a healthy state — that is the invariant.
	if psOut, psErr := compose().PsAll(); psErr == nil {
		for _, line := range strings.Split(psOut, "\n") {
			if strings.Contains(line, "tracker-a") {
				assert.NotContains(t, line, "(healthy)",
					"tracker-a must never be healthy on a corrupted chain (ps line: %q)", strings.TrimSpace(line))
			}
		}
	}

	// Restore happens in the t.Cleanup registered above.
}

// restoreTrackerA is scenario 9's cleanup: restore the pre-corruption
// ledger backup and bring tracker-a back up healthy on its ORIGINAL
// chain. Deliberately uses t.Errorf (never require.*/t.Fatalf/eventually)
// — it can run while the test goroutine is already unwinding from an
// earlier require.NoError failure, and re-entering FailNow/Goexit from
// that state is not something to rely on.
func restoreTrackerA(t *testing.T, ctx context.Context) {
	t.Helper()

	// Best-effort restore via a one-shot `docker compose run` against the
	// same named volume — tracker-a's own container may currently be
	// exited (that's the point of the tripwire test), so `exec` (which
	// requires a running container) isn't an option here. If no backup
	// was ever taken (e.g. the backup step itself failed before
	// corrupting anything), the `[ -f ... ]` guard makes this a no-op.
	restoreScript := "if [ -f " + ledgerDBPath + ".bak ]; then " +
		"cp -f " + ledgerDBPath + ".bak " + ledgerDBPath + " && " +
		"rm -f " + ledgerDBPath + "-wal " + ledgerDBPath + "-shm " + ledgerDBPath + ".bak; " +
		"fi"
	if out, err := compose().Run("tracker-a", "sh", "-c", restoreScript); err != nil {
		t.Errorf("e2e: scenario 9 cleanup: restore ledger DB backup: %v\n%s", err, out)
	}

	if err := compose().Start("tracker-a"); err != nil {
		t.Errorf("e2e: scenario 9 cleanup: start tracker-a after restore: %v", err)
	}

	if !pollUntilTrue(30*time.Second, 1*time.Second, func() bool {
		h, herr := adminA().Health(ctx)
		return herr == nil && h.Status == "ok"
	}) {
		t.Errorf("e2e: scenario 9 cleanup: tracker-a did not become healthy again after restoring the ledger backup")
		return
	}

	// Confirm the restored chain doesn't just report "healthy" but
	// actually re-passed the SAME startup gate scenario 9 exercised.
	if !pollUntilTrue(15*time.Second, 1*time.Second, func() bool {
		logs, logErr := compose().Logs("tracker-a")
		return logErr == nil && strings.Contains(logs, "ledger integrity verified at startup")
	}) {
		t.Errorf("e2e: scenario 9 cleanup: tracker-a logs did not show integrity verified after restoring the ledger backup")
	}
}

// TestScenario10_GracefulDrain is scenario 10 (Task 26 Step 3): SIGTERM
// must trigger a graceful shutdown (cmd/token-bay-tracker/run_cmd.go's
// signal.NotifyContext path — ctx.Done() drains srv/adminSrv/metricsSrv
// within cfg.Server.ShutdownGraceS, then RunE returns nil so main.go
// exits 0), not a crash. Distinguishing a clean exit 0 from a crash is
// the entire point of this scenario, so it asserts the concrete exit
// code rather than just "eventually healthy again".
func TestScenario10_GracefulDrain(t *testing.T) {
	ctx := context.Background()

	// Guarantee tracker-a is healthy again before this test returns,
	// regardless of where an assertion below fails — same t.Cleanup
	// pattern as scenario 9's restoreTrackerA (Go's testing package runs
	// t.Cleanup funcs even after a require.* failure unwinds the test).
	t.Cleanup(func() {
		if err := compose().Start("tracker-a"); err != nil {
			t.Logf("e2e: scenario 10 cleanup: compose start tracker-a: %v", err)
		}
		if !pollUntilTrue(30*time.Second, 1*time.Second, func() bool {
			h, herr := adminA().Health(ctx)
			return herr == nil && h.Status == "ok"
		}) {
			t.Errorf("e2e: scenario 10 cleanup: tracker-a did not return healthy after the SIGTERM drain")
		}
	})

	h, err := adminA().Health(ctx)
	require.NoError(t, err, "tracker-a should be healthy before this scenario touches it")
	require.Equal(t, "ok", h.Status)

	statsBefore, err := adminA().Stats(ctx)
	require.NoError(t, err)
	require.NotNil(t, statsBefore.Ledger.TipSeq)
	tipBefore := *statsBefore.Ledger.TipSeq

	require.NoError(t, compose().Kill("tracker-a", "SIGTERM"), "docker compose kill -s SIGTERM tracker-a")

	// Kill only delivers the signal; it doesn't wait for the container to
	// actually exit. Poll `ps -a` for the resulting container state.
	// cfg.Server.ShutdownGraceS defaults to 30s (internal/config/config.go),
	// so give comfortable headroom above that rather than assume a fast
	// exit — a crash would be fast, but a genuinely graceful drain is
	// allowed to take up to the full grace window.
	containerName := composeProjectID + "-tracker-a-1"
	var exitLine string
	require.True(t, pollUntilTrue(45*time.Second, 1*time.Second, func() bool {
		out, psErr := compose().PsAll()
		if psErr != nil {
			return false
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, containerName) && strings.Contains(line, "Exited") {
				exitLine = line
				return true
			}
		}
		return false
	}), "tracker-a should exit (not hang) within the shutdown grace period after SIGTERM")
	assert.Contains(t, exitLine, "Exited (0)", "SIGTERM should trigger a CLEAN exit (code 0) — a graceful drain, not a crash")

	// Bring tracker-a back up. This chain was never touched, so the
	// integrity gate must pass unconditionally here.
	require.NoError(t, compose().Start("tracker-a"), "restart tracker-a after the graceful SIGTERM drain")

	require.True(t, pollUntilTrue(30*time.Second, 1*time.Second, func() bool {
		hh, herr := adminA().Health(ctx)
		return herr == nil && hh.Status == "ok"
	}), "tracker-a /health should report ok after the SIGTERM drain + restart")

	require.True(t, pollUntilTrue(15*time.Second, 1*time.Second, func() bool {
		logs, logErr := compose().Logs("tracker-a")
		return logErr == nil && strings.Contains(logs, "ledger integrity verified at startup")
	}), "tracker-a logs should show integrity verified after the SIGTERM drain + restart")

	statsAfter, err := adminA().Stats(ctx)
	require.NoError(t, err)
	require.NotNil(t, statsAfter.Ledger.TipSeq)
	assert.Equal(t, tipBefore, *statsAfter.Ledger.TipSeq, "ledger tip_seq must be unchanged across a graceful SIGTERM drain + restart")
}

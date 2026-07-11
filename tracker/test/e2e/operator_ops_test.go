//go:build e2e

// Scenario 30: the operator's per-actor admission drill-down triage. Where
// scenario 25 covers the admission dashboards, blocklist, freeze and
// snapshot, this covers the per-identity views (GET /admission/consumer/{id},
// GET /admission/seeder/{id}) and the manual recompute
// (POST /admission/recompute/{id}) — specifically their triage contract.
//
// Admission's per-actor state is fed by the ledger event bus, which is not
// yet wired into admission (today the broker calls reputation.OnLedgerEvent
// directly; admission.OnLedgerEvent lands with the event-bus plan — see
// internal/reputation/CLAUDE.md). So an operator drilling into an actor that
// admission is not (yet) tracking must get a clean 404 — the same
// triage-friendly "not found, never a 5xx" contract as an unknown identity's
// zero-balance lookup (scenario 25) — and a malformed id must be rejected up
// front with 400 before any store lookup. This pins that behaviour so a
// future admission ledger-event wiring cannot silently turn a 404 into a 500.
package e2e_test

import (
	"context"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScenario30_OperatorAdmissionPerActorTriage(t *testing.T) {
	// A well-formed identity admission has never seen: the per-actor views and
	// the manual recompute all answer 404, not a server error.
	untracked := randomHex(t, 32)

	_, err := adminA().AdmissionSeeder(t.Context(), untracked)
	requireAdminStatus(t, err, http.StatusNotFound, "GET /admission/seeder for an untracked seeder")

	_, err = adminA().AdmissionConsumer(t.Context(), untracked)
	requireAdminStatus(t, err, http.StatusNotFound, "GET /admission/consumer for an untracked consumer")

	_, err = adminA().AdmissionRecompute(t.Context(), untracked)
	requireAdminStatus(t, err, http.StatusNotFound, "POST /admission/recompute for an untracked consumer")

	// Malformed identity hex is rejected with 400 before the store lookup —
	// the operator fat-fingered the id, and the tracker says so cleanly.
	_, err = adminA().AdmissionConsumer(t.Context(), "not-hex")
	requireAdminStatus(t, err, http.StatusBadRequest, "GET /admission/consumer with a malformed id")

	_, err = adminA().AdmissionRecompute(t.Context(), "zz")
	requireAdminStatus(t, err, http.StatusBadRequest, "POST /admission/recompute with a malformed id")
}

// TestScenario32_AdmissionTracksFromEnrollment is the positive counterpart to
// scenario 30: with the starter-grant ledger-event wiring in place, admission
// begins tracking a consumer the moment it enrolls — no settlement required.
// A fresh identity that has done nothing but enroll (and receive its starter
// grant) must already appear in admission's per-consumer view, flipping
// scenario 30's untracked 404 to a populated 200 for a brand-new actor.
func TestScenario32_AdmissionTracksFromEnrollment(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cli := dialTrackerA(ctx, t)
	enrollClient(ctx, t, cli) // enroll → starter grant → admission.OnLedgerEvent
	id := cli.IdentityID()
	idHex := hex.EncodeToString(id[:])

	// The enroll-time dispatch is synchronous with the enroll RPC, but the
	// admin read races the tracker's own goroutines, so poll briefly.
	eventually(t, 15*time.Second, 500*time.Millisecond,
		"admission tracks the freshly-enrolled consumer via the starter-grant wiring", func() bool {
			_, e := adminA().AdmissionConsumer(ctx, idHex)
			return e == nil
		})
	adm, err := adminA().AdmissionConsumer(ctx, idHex)
	require.NoError(t, err, "GET /admission/consumer/{id} for a freshly-enrolled consumer")
	assert.Contains(t, adm, "score", "enrolled consumer's admission view should carry its local score")
}

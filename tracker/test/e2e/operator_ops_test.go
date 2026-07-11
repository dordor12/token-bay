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
	"net/http"
	"testing"
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

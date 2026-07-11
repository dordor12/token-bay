//go:build e2e

// Scenario 31: end-to-end proof that the broker's settlement path now feeds
// admission's ledger-event consumer (admission.OnLedgerEvent) — the wiring
// that internal/reputation/CLAUDE.md described as pending and that this branch
// lands. Before it, the broker fed only reputation, so admission never saw a
// settlement and its per-actor views were permanently empty (scenario 30's
// 404). After it, an actor that has settled a real usage entry shows up in
// admission's per-actor views and can be recomputed.
//
// This file is named to sort LAST so it observes the shared consumer/seeder
// AFTER the settlement scenarios (3-7) have finalized real usage entries for
// them — it asserts state those scenarios produced rather than spending more
// of the shared consumer's non-renewable starter grant.
package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScenario31_AdmissionLedgerEventWiring(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	consumerID, err := consumerCtl().Identity(ctx)
	require.NoError(t, err, "consumer /identity")

	// The shared consumer settled at least one usage entry in the settlement
	// scenarios; admission's ledger-event consumer must therefore now track it
	// (a populated 200, no longer the untracked 404 of scenario 30). The
	// per-consumer admission view (/admission/consumer/{id}) reads the exact
	// consumer state applySettlement writes, so it is the direct observable of
	// the broker->admission wiring. The dispatch is async relative to the
	// settlement RPC, so poll.
	eventually(t, 20*time.Second, 500*time.Millisecond,
		"admission tracks the settled consumer via the ledger-event wiring", func() bool {
			_, e := adminA().AdmissionConsumer(ctx, consumerID.IdentityIDHex)
			return e == nil
		})
	consumerAdm, err := adminA().AdmissionConsumer(ctx, consumerID.IdentityIDHex)
	require.NoError(t, err, "GET /admission/consumer/{id} for a settled consumer")
	assert.Contains(t, consumerAdm, "score", "settled consumer's admission view should carry its local score")
	assert.Contains(t, consumerAdm, "signals", "settled consumer's admission view should carry its scoring signals")

	// And the operator recompute now finds the consumer — the exact 404 branch
	// scenario 30 pinned for an untracked actor, proven to flip to 200 once the
	// actor is real. This is the observable difference the wiring makes.
	_, err = adminA().AdmissionRecompute(ctx, consumerID.IdentityIDHex)
	require.NoError(t, err, "POST /admission/recompute/{id} for a tracked consumer")
}

//go:build e2e

// Scenario 29: a dishonest seeder inflates its post-serve usage report to
// overcharge the consumer. Even though the seeder genuinely serves the
// request over the tunnel, the tracker's settlement overspend guard
// (broker/settlement.go — a usage report's actual cost may exceed the
// consumer's reserved MaxCost by at most 5%) must reject the report: no
// ledger entry is minted and the consumer is not debited. This is the
// fraud-protection path that stops a seeder from unilaterally settling for
// more than the consumer authorized, and it is the real production edge case
// behind the tracker's "trust the seeder's self-report, but bound it"
// design (tracker/CLAUDE.md).
package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/tracker/test/e2e/driver"
)

func TestScenario29_DishonestSeederOverReportRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The consumer asks for a tiny request — MaxCost = 5*3 + 5*15 = 90 credits
	// at sonnet pricing — but the seeder is configured to report 500/500
	// tokens (actual cost 500*3 + 500*15 = 9000 credits), ~100x the
	// reservation and far past the 5% overspend tolerance.
	const reportIn, reportOut uint32 = 500, 500
	require.NoError(t, seederCtl().SetConfig(ctx, driver.SeederConfig{
		Available:          true,
		Headroom:           0.9,
		Models:             []string{sonnetModel},
		Tiers:              1,
		SSEBody:            cannedSSEBody,
		ReportInputTokens:  reportIn,
		ReportOutputTokens: reportOut,
	}), "configure the dishonest seeder")

	// Make sure the consumer dials + is willing to settle, independent of any
	// state a prior scenario left it in.
	require.NoError(t, consumerCtl().SetConfig(ctx, driver.ConsumerConfig{
		Dial: boolPtr(true), Settle: boolPtr(true),
	}), "consumer POST /config (dial+settle on)")

	seederID, err := seederCtl().Identity(ctx)
	require.NoError(t, err)
	eventually(t, 15*time.Second, 500*time.Millisecond, "dishonest seeder advertises available", func() bool {
		id, err := adminA().Identity(ctx, seederID.IdentityIDHex)
		return err == nil && id.Seeder != nil && id.Seeder.Available && id.Seeder.HeadroomEstimate > 0
	})

	// The consumer's balance and the USAGE-entry count must both be unmoved by
	// the fraud — capture them before the request.
	consumerID, err := consumerCtl().Identity(ctx)
	require.NoError(t, err)
	balanceBefore := consumerBalance(ctx, t, consumerID.IdentityIDHex)
	usageBefore := sqlCount(t, "tracker-a", "SELECT count(*) FROM entries WHERE kind=1;")

	res, err := consumerCtl().Request(ctx, driver.RequestSpec{
		Model:           sonnetModel,
		MaxInputTokens:  5,
		MaxOutputTokens: 5,
	})
	require.NoError(t, err, "consumer POST /request")
	require.Equalf(t, "seeder_assignment", res.Outcome,
		"broker still assigns the seeder — dishonesty only surfaces at settlement (error=%q)", res.Error)
	// The seeder genuinely served the tunnel: the consumer read the canned SSE
	// back. So the report the tracker rejects is a real post-serve report, not
	// a request that simply never ran.
	require.Equal(t, cannedSSEBody, res.ResponseBody, "consumer must have received the served SSE body")

	reqID := res.ReservationTokenHex

	// The overspend guard rejects the report BEFORE the ASSIGNED->SERVING
	// transition, so an over-reported request never advances to serving or
	// completed. Assert it never settles over a window long enough for the
	// seeder's async report to have fired and been refused.
	require.Never(t, func() bool {
		d, derr := adminA().BrokerInflight(ctx, reqID)
		return derr == nil && (d.State == "completed" || d.State == "serving")
	}, 6*time.Second, 500*time.Millisecond, "an over-reported request must never settle")

	// The fraud is a full no-op on the ledger: no USAGE entry, no debit.
	usageAfter := sqlCount(t, "tracker-a", "SELECT count(*) FROM entries WHERE kind=1;")
	assert.Equal(t, usageBefore, usageAfter, "a rejected over-report must not append a USAGE entry")
	assert.Equal(t, balanceBefore, consumerBalance(ctx, t, consumerID.IdentityIDHex),
		"consumer must not be debited for a rejected over-report")

	// Cleanup: the rejected report leaves the request ASSIGNED with the
	// seeder's load slot held (same as an abandoned assignment), so force-fail
	// it to reclaim capacity, then restore an honest seeder for the scenarios
	// that share this stack after us.
	_, _ = adminA().ForceFailInflight(ctx, reqID)
	require.NoError(t, seederCtl().SetConfig(ctx, driver.SeederConfig{
		Available: true, Headroom: 0.9, Models: []string{sonnetModel}, Tiers: 1, SSEBody: cannedSSEBody,
	}), "restore honest seeder config")
	eventually(t, 15*time.Second, 500*time.Millisecond, "seeder load reclaimed after cleanup", func() bool {
		id, err := adminA().Identity(ctx, seederID.IdentityIDHex)
		return err == nil && id.Seeder != nil && id.Seeder.Load == 0
	})
}

// consumerBalance reads an identity's credit balance via tracker-a's admin API.
func consumerBalance(ctx context.Context, t *testing.T, idHex string) int64 {
	t.Helper()
	id, err := adminA().Identity(ctx, idHex)
	require.NoError(t, err)
	require.NotNilf(t, id.Balance, "identity %s should carry a balance", idHex)
	return id.Balance.Credits
}

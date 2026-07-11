//go:build e2e

// Scenario 35: the concurrent multi-seeder behavior matrix — the "full
// concurrent setup" the testcontainers migration unlocks. It runs TWO seeders
// with DIFFERENT behaviors at the same instant (a capability the single-seeder
// compose topology could not express) and drives a consumer at both
// simultaneously, using model-based routing to pin each consumer to its
// intended seeder:
//
//   - sonnet -> the honest compose seeder: serves and settles a real USAGE entry.
//   - opus   -> a dynamically-added over-reporting seeder: serves, then inflates
//     its usage report so the tracker's overspend guard rejects it.
//
// The two flows are served concurrently by two different seeder actors (two
// container IPs, two tunnel listeners), and the tracker must keep each correct:
// exactly one honest settlement lands on the ledger, the fraud lands nothing,
// and neither consumer's outcome is poisoned by the other running in flight.
package e2e_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/tracker/test/e2e/driver"
)

const opusModel = "claude-opus-4-7"

func TestScenario35_ConcurrentMultiSeederBehaviorMatrix(t *testing.T) {
	if stack() == nil {
		t.Skip("dynamic multi-seeder matrix requires the testcontainers backend (not E2E_REUSE_STACK)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	// seeder1 = the honest compose seeder, advertising sonnet.
	require.NoError(t, seederCtl().SetConfig(ctx, driver.SeederConfig{
		Available: true, Headroom: 0.9, Models: []string{sonnetModel}, Tiers: 1, SSEBody: cannedSSEBody,
	}), "configure the honest sonnet seeder")

	// seeder2 = a dynamically-added over-reporting seeder, advertising opus.
	seeder2, stop2, err := stack().StartSeeder(ctx, "seeder2")
	require.NoError(t, err, "start the dynamic over-report seeder")
	defer stop2()
	require.NoError(t, seeder2.SetConfig(ctx, driver.SeederConfig{
		Available: true, Headroom: 0.9, Models: []string{opusModel}, Tiers: 1, SSEBody: cannedSSEBody,
		ReportInputTokens: 500, ReportOutputTokens: 500,
	}), "configure the over-report opus seeder")

	s1id, err := seederCtl().Identity(ctx)
	require.NoError(t, err)
	s2id, err := seeder2.Identity(ctx)
	require.NoError(t, err)
	eventually(t, 40*time.Second, 500*time.Millisecond, "both seeders advertised to tracker-a", func() bool {
		r1, e1 := adminA().Identity(ctx, s1id.IdentityIDHex)
		r2, e2 := adminA().Identity(ctx, s2id.IdentityIDHex)
		return e1 == nil && r1.Seeder != nil && r1.Seeder.Available &&
			e2 == nil && r2.Seeder != nil && r2.Seeder.Available
	})

	require.NoError(t, consumerCtl().SetConfig(ctx, driver.ConsumerConfig{
		Dial: boolPtr(true), Settle: boolPtr(true),
	}), "consumer dial+settle on")

	usageBefore := sqlCount(t, "tracker-a", "SELECT count(*) FROM entries WHERE kind=1;")

	type reqOut struct {
		body       string
		seederAddr string
		outcome    string
		reqID      string
		err        error
	}
	var wg sync.WaitGroup
	var happy, fraud reqOut
	wg.Add(2)
	// sonnet -> honest seeder.
	go func() {
		defer wg.Done()
		r, e := consumerCtl().Request(ctx, driver.RequestSpec{Model: sonnetModel, MaxInputTokens: 1, MaxOutputTokens: 1})
		if e != nil {
			happy.err = e
			return
		}
		happy = reqOut{body: r.ResponseBody, seederAddr: r.SeederAddr, outcome: r.Outcome, reqID: r.ReservationTokenHex}
	}()
	// opus -> over-report seeder.
	go func() {
		defer wg.Done()
		r, e := consumerCtl().Request(ctx, driver.RequestSpec{Model: opusModel, MaxInputTokens: 1, MaxOutputTokens: 1})
		if e != nil {
			fraud.err = e
			return
		}
		fraud = reqOut{body: r.ResponseBody, seederAddr: r.SeederAddr, outcome: r.Outcome, reqID: r.ReservationTokenHex}
	}()
	wg.Wait()

	require.NoError(t, happy.err, "sonnet consumer request")
	require.NoError(t, fraud.err, "opus consumer request")
	require.Equal(t, "seeder_assignment", happy.outcome, "sonnet consumer assigned")
	require.Equal(t, "seeder_assignment", fraud.outcome, "opus consumer assigned")

	// Both were genuinely served (non-empty tunnel body) — and by DIFFERENT
	// seeders, concurrently: model routing sent each to its behavior class.
	assert.Equal(t, cannedSSEBody, happy.body, "sonnet consumer served by the honest seeder")
	assert.Equal(t, cannedSSEBody, fraud.body, "opus consumer served by the over-report seeder")
	assert.NotEqual(t, happy.seederAddr, fraud.seederAddr,
		"the two behaviors must be served by DIFFERENT seeders concurrently (got same addr %q)", happy.seederAddr)

	// Terminal states diverge correctly: the honest sonnet request settles;
	// the over-report opus request is rejected and never settles.
	waitForInflightState(ctx, t, happy.reqID, "completed", 25*time.Second)
	require.Never(t, func() bool {
		d, e := adminA().BrokerInflight(ctx, fraud.reqID)
		return e == nil && (d.State == "completed" || d.State == "serving")
	}, 6*time.Second, 500*time.Millisecond, "the over-report request must never settle")

	// Exactly one new USAGE entry landed — the honest one. The concurrent
	// fraud minted nothing.
	usageAfter := sqlCount(t, "tracker-a", "SELECT count(*) FROM entries WHERE kind=1;")
	assert.Equal(t, usageBefore+1, usageAfter, "only the honest concurrent settlement appends a USAGE entry")
	t.Logf("e2e: scenario 35: honest sonnet settled on %s, over-report opus rejected on %s (concurrent)",
		happy.seederAddr, fraud.seederAddr)

	// Reclaim the abandoned fraud request's load AND its held credit
	// reservation (force-fail frees only the load) so the shared consumer's
	// budget is not drained for later scenarios.
	_, _ = adminA().ForceFailInflight(ctx, fraud.reqID)
	_, _ = adminA().ForceReleaseReservation(ctx, fraud.reqID)
}

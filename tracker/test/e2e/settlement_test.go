//go:build e2e

package e2e_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/tracker/test/e2e/driver"
)

// Scenarios 3-7 (docs/superpowers/plans/2026-07-08-tracker-e2e-testing.md
// Task 25) exercise the headline consumer->seeder->settlement flow: broker
// assignment, the tunnel data plane, the settlement happy path, the
// dispute/timeout path, and the abandoned-assignment reservation reaper.
// They share the single seeder/consumer pair TestMain brings up once for
// the whole package, so each test picks its own MaxInputTokens/
// MaxOutputTokens deliberately small — the consumer's starter grant
// (scenario 2: exactly 1000 credits) is a hard, non-renewable budget shared
// across every scenario in this file, and sonnet pricing is 3 credits/input
// token + 15 credits/output token (tracker/internal/config's default price
// table, mirrored by e2egen and the seeder actor). Budget used by this
// file: scenario3 180 + scenario4 180 + scenario5 360 + scenario6 90 = 810
// credits, comfortably under 1000 with room for scenario7's 90-credit
// reservation (never debited — it's abandoned before any usage report).
const (
	sonnetModel = "claude-sonnet-4-6"

	// cannedSSEBody is the fixed tunnel response the seeder is configured
	// (scenario 3) to serve for every offer in this file.
	cannedSSEBody = "data: {\"type\":\"message_stop\"}\n\n"

	scenario3MaxInputTokens, scenario3MaxOutputTokens = 10, 10 // cost 180
	scenario4MaxInputTokens, scenario4MaxOutputTokens = 10, 10 // cost 180
	scenario5MaxInputTokens, scenario5MaxOutputTokens = 20, 20 // cost 360
	scenario6MaxInputTokens, scenario6MaxOutputTokens = 5, 5   // cost 90
	scenario7MaxInputTokens, scenario7MaxOutputTokens = 5, 5   // cost 90 (reserved, never debited)
)

// boolPtr is the driver.ConsumerConfig{Settle,Dial} toggle helper — both
// fields are *bool so POST /config can distinguish "unset" (nil, actor
// default true) from an explicit false.
func boolPtr(b bool) *bool { return &b }

// nonTerminalOrDoneStates are the broker session.State labels a request can
// legitimately be in immediately after a "seeder_assignment" outcome:
// selection has already advanced past StateSelecting server-side (the
// broker only returns an assignment to the caller after the offer is
// accepted and the state machine transitions SELECTING->ASSIGNED), and the
// settlement round trip that follows may have already advanced it further
// by the time the test observes it.
var nonTerminalOrDoneStates = []string{"assigned", "serving", "completed"}

// configureAndWaitForSeederAdvertise is scenario 3's setup step, factored
// out because scenario 3 is the only scenario that (re)configures the
// seeder — 4 through 7 reuse the same advertised seeder for the rest of
// this file's TestMain-managed stack lifetime.
func configureAndWaitForSeederAdvertise(ctx context.Context, t *testing.T) {
	t.Helper()
	require.NoError(t, seederCtl().SetConfig(ctx, driver.SeederConfig{
		Available: true,
		Headroom:  0.9,
		Models:    []string{sonnetModel},
		Tiers:     1, // bit0 = PRIVACY_TIER_STANDARD, matching envelopebuilder's fixed tier
		SSEBody:   cannedSSEBody,
	}), "seeder POST /config")

	seederID, err := seederCtl().Identity(ctx)
	require.NoError(t, err, "seeder /identity")

	// The advertise loop is kicked immediately by /config but the RPC to
	// tracker-a's registry is still async relative to this HTTP call
	// returning — poll rather than sleep a fixed duration.
	eventually(t, 15*time.Second, 500*time.Millisecond,
		"tracker-a registry to show the seeder available with positive headroom",
		func() bool {
			id, err := adminA().Identity(ctx, seederID.IdentityIDHex)
			if err != nil || id.Seeder == nil {
				return false
			}
			return id.Seeder.Available && id.Seeder.HeadroomEstimate > 0
		})
}

// TestScenario03_BrokerAssignment is scenario 3 (Task 25 Step 1): a
// consumer request against an available, matching-model, matching-tier
// seeder is routed by the broker to a "seeder_assignment" outcome carrying
// a non-empty seeder address/pubkey/reservation token, and tracker-a's
// admin API reflects a non-empty in-flight state for that reservation.
func TestScenario03_BrokerAssignment(t *testing.T) {
	ctx := context.Background()

	configureAndWaitForSeederAdvertise(ctx, t)

	res, err := consumerCtl().Request(ctx, driver.RequestSpec{
		Model:           sonnetModel,
		MaxInputTokens:  scenario3MaxInputTokens,
		MaxOutputTokens: scenario3MaxOutputTokens,
	})
	require.NoError(t, err, "consumer POST /request")
	require.Equal(t, "seeder_assignment", res.Outcome, "unexpected outcome (error=%q)", res.Error)
	assert.NotEmpty(t, res.SeederAddr, "assignment must carry the seeder's tunnel-reachable address")
	assert.NotEmpty(t, res.SeederPubkeyHex, "assignment must carry the seeder's ephemeral pubkey")
	assert.NotEmpty(t, res.ReservationTokenHex, "assignment must carry the reservation token (= request_id)")

	detail, err := adminA().BrokerInflight(ctx, res.ReservationTokenHex)
	require.NoError(t, err, "tracker-a GET /broker/inflight/%s", res.ReservationTokenHex)
	assert.NotEmpty(t, detail.State, "in-flight state should not be empty once a seeder is assigned")
	assert.Contains(t, nonTerminalOrDoneStates, detail.State,
		"in-flight state should be assigned/serving/completed, got %q", detail.State)

	// Drain to "completed" before returning, even though the assertions
	// above are already satisfied by "assigned": the seeder actor's
	// serveOffer goroutine still has async work outstanding at this point
	// (send the UsageReport RPC, then close its tunnel listener), and
	// scenario 4 immediately reuses the SAME fixed --tunnel-addr port for
	// its own offer. Returning from this test while that cleanup is still
	// in flight races scenario 4's tunnel.Listen against this request's
	// still-open listener on that port — waiting here for the settlement
	// to fully land removes the race instead of relying on scenario 4 to
	// tolerate it.
	waitForInflightState(ctx, t, res.ReservationTokenHex, "completed", 15*time.Second)
}

// TestScenario04_TunnelDataPlane is scenario 4 (Task 25 Step 2): a fresh
// request's ResponseBody equals the seeder's configured canned SSE body
// exactly, proving the consumer dialed the seeder's tunnel (deriving its
// address via the broker's SeederAddr host + the fixed --seeder-tunnel-port
// substitution), authenticated the ephemeral-key handshake, sent the
// request, and read the SSE bytes back — the full data-plane round trip in
// Docker, not just the control-plane assignment scenario 3 already proved.
func TestScenario04_TunnelDataPlane(t *testing.T) {
	ctx := context.Background()

	res, err := consumerCtl().Request(ctx, driver.RequestSpec{
		Model:           sonnetModel,
		MaxInputTokens:  scenario4MaxInputTokens,
		MaxOutputTokens: scenario4MaxOutputTokens,
	})
	require.NoError(t, err, "consumer POST /request")
	require.Equal(t, "seeder_assignment", res.Outcome, "unexpected outcome (error=%q)", res.Error)
	assert.Equal(t, cannedSSEBody, res.ResponseBody,
		"tunnel round-trip must return the seeder's configured canned SSE body verbatim")
}

// TestScenario05_SettlementHappyPath is scenario 5 (Task 25 Step 3): after
// a served request, the in-flight state reaches "completed", the ledger
// gains a USAGE (kind=1) entry with a non-empty consumer_sig (the P4 happy
// path — both participants signed the usage-assertion), and the balances
// projection reflects the debit (consumer) and credit (seeder).
func TestScenario05_SettlementHappyPath(t *testing.T) {
	ctx := context.Background()

	seederID, err := seederCtl().Identity(ctx)
	require.NoError(t, err, "seeder /identity")
	seederBefore := waitForBalance(ctx, t, seederID.IdentityIDHex)

	consumerBefore, err := consumerCtl().Balance(ctx)
	require.NoError(t, err, "consumer GET /balance")

	res, err := consumerCtl().Request(ctx, driver.RequestSpec{
		Model:           sonnetModel,
		MaxInputTokens:  scenario5MaxInputTokens,
		MaxOutputTokens: scenario5MaxOutputTokens,
	})
	require.NoError(t, err, "consumer POST /request")
	require.Equal(t, "seeder_assignment", res.Outcome, "unexpected outcome (error=%q)", res.Error)

	waitForInflightState(ctx, t, res.ReservationTokenHex, "completed", 15*time.Second)

	assertLedgerEntryCount(t,
		"SELECT count(*) FROM entries WHERE kind=1 AND consumer_sig IS NOT NULL AND length(consumer_sig)>0;",
		"expected at least one USAGE entry with a non-empty consumer_sig (P4 happy path)")

	consumerAfter, err := consumerCtl().Balance(ctx)
	require.NoError(t, err, "consumer GET /balance")
	assert.Less(t, consumerAfter.Credits, consumerBefore.Credits, "consumer should be debited by the settled usage cost")

	seederAfter := waitForBalanceAbove(ctx, t, seederID.IdentityIDHex, seederBefore.Balance.Credits)
	assert.Greater(t, seederAfter.Balance.Credits, seederBefore.Balance.Credits, "seeder should be credited by the settled usage cost")
}

// TestScenario06_SettlementDispute is scenario 6 (Task 25 Step 4): the
// consumer is configured to stay silent on the pushed SettlementRequest
// (POST /config {settle:false}); after settlement_timeout_s (3s, e2egen's
// tuned config) elapses with no counter-signature, the tracker's timer
// fallback still appends a USAGE entry — this time with an EMPTY
// consumer_sig — and the balances still move (the seeder's signed usage
// report alone is sufficient to settle; the consumer's counter-signature is
// an accountability record, not a settlement precondition).
func TestScenario06_SettlementDispute(t *testing.T) {
	ctx := context.Background()

	require.NoError(t, consumerCtl().SetConfig(ctx, driver.ConsumerConfig{Settle: boolPtr(false)}), "consumer POST /config {settle:false}")
	t.Cleanup(func() {
		require.NoError(t, consumerCtl().SetConfig(context.Background(), driver.ConsumerConfig{Settle: boolPtr(true)}), "reset consumer settle=true")
	})

	seederID, err := seederCtl().Identity(ctx)
	require.NoError(t, err, "seeder /identity")
	seederBefore := waitForBalance(ctx, t, seederID.IdentityIDHex)

	consumerBefore, err := consumerCtl().Balance(ctx)
	require.NoError(t, err, "consumer GET /balance")

	res, err := consumerCtl().Request(ctx, driver.RequestSpec{
		Model:           sonnetModel,
		MaxInputTokens:  scenario6MaxInputTokens,
		MaxOutputTokens: scenario6MaxOutputTokens,
	})
	require.NoError(t, err, "consumer POST /request")
	require.Equal(t, "seeder_assignment", res.Outcome, "unexpected outcome (error=%q)", res.Error)

	// Bounded well above settlement_timeout_s(3s): the tracker's timer
	// fallback only fires after that timeout elapses with no counter-sig,
	// then appends and transitions ASSIGNED/SERVING->COMPLETED — poll for
	// that transition instead of sleeping a fixed 3s+margin.
	waitForInflightState(ctx, t, res.ReservationTokenHex, "completed", 15*time.Second)

	settlement, err := consumerCtl().LastSettlement(ctx)
	require.NoError(t, err, "consumer GET /settlement/last")
	assert.False(t, settlement.Signed, "consumer must have refused to counter-sign (settle=false)")

	assertLedgerEntryCount(t,
		"SELECT count(*) FROM entries WHERE kind=1 AND (consumer_sig IS NULL OR length(consumer_sig)=0);",
		"expected at least one USAGE entry with an empty consumer_sig (dispute/timeout path)")

	consumerAfter, err := consumerCtl().Balance(ctx)
	require.NoError(t, err, "consumer GET /balance")
	assert.Less(t, consumerAfter.Credits, consumerBefore.Credits, "consumer should still be debited despite the dispute")

	seederAfter := waitForBalanceAbove(ctx, t, seederID.IdentityIDHex, seederBefore.Balance.Credits)
	assert.Greater(t, seederAfter.Balance.Credits, seederBefore.Balance.Credits, "seeder should still be credited despite the dispute")
}

// TestScenario07_AbandonedAssignment is scenario 7 (Task 25 Step 5): the
// consumer is configured to never dial the assigned seeder's tunnel (POST
// /config {dial:false}). No usage report is ever sent, so the reservation
// just sits ASSIGNED until the broker's reservation reaper — a ticker
// HARDCODED to a 30s period (tracker/internal/broker/reaper.go) — sweeps it
// to StateFailed and releases the reservation slot. This is the single
// largest budget line in this file's scenarios: the reaper's own tick
// period puts a ~30-35s FLOOR on how soon this is observable, so the
// eventually() timeout below is set generously above that floor rather
// than tightened to "as fast as possible" like every other scenario's
// poll — there is no way to make this one fast without changing production
// reaper cadence, which is out of scope for an e2e test.
func TestScenario07_AbandonedAssignment(t *testing.T) {
	ctx := context.Background()

	require.NoError(t, consumerCtl().SetConfig(ctx, driver.ConsumerConfig{Dial: boolPtr(false)}), "consumer POST /config {dial:false}")
	t.Cleanup(func() {
		require.NoError(t, consumerCtl().SetConfig(context.Background(), driver.ConsumerConfig{Dial: boolPtr(true)}), "reset consumer dial=true")
	})

	res, err := consumerCtl().Request(ctx, driver.RequestSpec{
		Model:           sonnetModel,
		MaxInputTokens:  scenario7MaxInputTokens,
		MaxOutputTokens: scenario7MaxOutputTokens,
	})
	require.NoError(t, err, "consumer POST /request")
	require.Equal(t, "seeder_assignment", res.Outcome, "unexpected outcome (error=%q)", res.Error)
	assert.Empty(t, res.ResponseBody, "dial=false must never read a tunnel response body")

	// ~30-35s budget: the reaper's ticker period is hardcoded to 30s
	// (tracker/internal/broker/reaper.go), so this cannot observably
	// complete faster than that regardless of poll interval.
	waitForInflightState(ctx, t, res.ReservationTokenHex, "failed", 50*time.Second)

	reservations, err := adminA().Reservations(ctx)
	require.NoError(t, err, "tracker-a GET /broker/reservations")
	for _, c := range reservations {
		for _, slot := range c.Slots {
			assert.NotEqual(t, res.ReservationTokenHex, slot.RequestID,
				"reservation slot for the abandoned request should have been released by the reaper")
		}
	}
}

// waitForInflightState polls Admin(a).BrokerInflight(reqIDHex) until its
// State equals want or timeout elapses.
func waitForInflightState(ctx context.Context, t *testing.T, reqIDHex, want string, timeout time.Duration) {
	t.Helper()
	var last string
	eventually(t, timeout, 1*time.Second, "tracker-a /broker/inflight/"+reqIDHex+` state to reach "`+want+`"`, func() bool {
		d, err := adminA().BrokerInflight(ctx, reqIDHex)
		if err != nil {
			return false
		}
		last = d.State
		return d.State == want
	})
	t.Logf("request %s reached inflight state %q", reqIDHex, last)
}

// waitForBalance polls Admin(a).Identity(idHex) until it reports a signed
// balance snapshot, mirroring bringup_test.go's waitForStarterGrant but
// reusable here for both consumer and seeder identities mid-suite (not just
// at first-grant time).
func waitForBalance(ctx context.Context, t *testing.T, idHex string) *driver.IdentityResponse {
	t.Helper()
	var identity *driver.IdentityResponse
	eventually(t, 15*time.Second, 500*time.Millisecond, "Admin(a).Identity("+idHex+") balance to be populated", func() bool {
		id, err := adminA().Identity(ctx, idHex)
		if err != nil || id.Balance == nil {
			return false
		}
		identity = id
		return true
	})
	require.NotNil(t, identity, "Admin(a).Identity(%s) never returned a balance", idHex)
	return identity
}

// waitForBalanceAbove polls Admin(a).Identity(idHex) until its credits
// exceed floor (the pre-settlement snapshot), so callers observe the
// post-settlement credited balance rather than racing the async ledger
// append that follows a "completed" in-flight state by a beat.
func waitForBalanceAbove(ctx context.Context, t *testing.T, idHex string, floor int64) *driver.IdentityResponse {
	t.Helper()
	var identity *driver.IdentityResponse
	eventually(t, 10*time.Second, 500*time.Millisecond, "Admin(a).Identity("+idHex+") balance to rise above its pre-settlement value", func() bool {
		id, err := adminA().Identity(ctx, idHex)
		if err != nil || id.Balance == nil {
			return false
		}
		identity = id
		return id.Balance.Credits > floor
	})
	require.NotNil(t, identity, "Admin(a).Identity(%s) never returned a balance", idHex)
	return identity
}

// assertLedgerEntryCount runs a `SELECT count(*) ...` query against
// tracker-a's on-disk ledger via SQLiteQuery and asserts the count is > 0.
func assertLedgerEntryCount(t *testing.T, sql, msg string) {
	t.Helper()
	out, err := driver.SQLiteQuery(compose(), "tracker-a", "/data/ledger.sqlite", sql)
	require.NoError(t, err, "sqlite3 query on tracker-a ledger: %s", sql)
	n, convErr := strconv.Atoi(strings.TrimSpace(out))
	require.NoError(t, convErr, "sqlite3 output not an integer: %q", out)
	assert.Greater(t, n, 0, msg)
}

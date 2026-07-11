//go:build e2e

// Scenario 28: concurrent broker contention — the real-world case where
// many consumers hit one region at the same instant and compete for a
// limited pool of seeder capacity. This is the tracker's own concurrency
// under test: the broker's per-seeder load accounting (IncLoad/DecLoad +
// the LoadThreshold filter), reservation minting, and the ledger's append
// serialization all run simultaneously across N independent mTLS
// connections. The invariants that must hold regardless of interleaving:
// every request reaches a terminal outcome (no deadlock/starvation), no
// two winners share a reservation token (no double-assign), the seeder
// serves up to but not beyond its load cap, and the chain stays intact
// with exactly the starter grants — no phantom debit, no lost/duplicated
// entry. Each consumer is a throwaway host-side identity (driver.RPCClient),
// so the burst is genuinely concurrent, not serialized through one actor.
package e2e_test

import (
	"context"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/test/e2e/driver"
)

func TestScenario28_ConcurrentBrokerContention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// One happy seeder, small token asks so MaxCost fits the starter grant.
	require.NoError(t, seederCtl().SetConfig(ctx, driver.SeederConfig{
		Available: true, Headroom: 0.9, Models: []string{sonnetModel}, Tiers: 1,
		SSEBody: "data: {\"type\":\"message_stop\"}\n\n",
	}), "configure the happy seeder")
	// Give the advertise loop a beat to publish availability before the burst.
	require.Eventually(t, func() bool {
		sid, err := seederCtl().Identity(ctx)
		if err != nil {
			return false
		}
		rec, err := adminA().Identity(ctx, sid.IdentityIDHex)
		return err == nil && rec.Seeder != nil && rec.Seeder.Available
	}, 20*time.Second, 500*time.Millisecond, "seeder becomes available")

	// USAGE entries present before the burst (other scenarios in the shared
	// stack may have appended some); the abandoned burst must not add more.
	usageBefore := sqlCount(t, "tracker-a", "SELECT count(*) FROM entries WHERE kind=1;")

	// One outcome per concurrent consumer. testify's require.FailNow is
	// only safe on the test goroutine, so workers record an err instead and
	// the main goroutine checks it after the barrier.
	type outcome struct {
		assigned bool
		resToken string
		err      error
	}
	const n = 10
	results := make([]outcome, n)

	// A start barrier so all N broker_requests race, not trickle: each
	// worker enrolls first (serially-safe, independent identities), then
	// blocks on `gun` until they all fire together.
	var ready sync.WaitGroup
	ready.Add(n)
	gun := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(n)

	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			cli := dialTrackerA(ctx, t)
			enrollClient(ctx, t, cli)
			snap, err := cli.VerifiedBalance(ctx, cli.IdentityID())
			if err != nil {
				results[i] = outcome{err: err}
				ready.Done()
				<-gun
				return
			}
			env := buildSignedBrokerEnvelope(t, cli, sonnetModel, 10, 10, snap)

			ready.Done()
			<-gun // fire together

			resp, err := cli.Call(ctx, tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST, env)
			if err != nil {
				results[i] = outcome{err: err}
				return
			}
			if resp.Status == tbproto.RpcStatus_RPC_STATUS_OK {
				var brr tbproto.BrokerRequestResponse
				if err := proto.Unmarshal(resp.Payload, &brr); err != nil {
					results[i] = outcome{err: err}
					return
				}
				if sa := brr.GetSeederAssignment(); sa != nil {
					results[i] = outcome{assigned: true, resToken: hex.EncodeToString(sa.ReservationToken)}
				}
			}
		}(i)
	}

	ready.Wait()
	close(gun) // release the burst
	wg.Wait()  // LIVENESS: every worker returned within the ctx deadline (no deadlock)

	// Correctness under the interleaving.
	assigned := 0
	tokens := map[string]int{}
	for i, r := range results {
		require.NoError(t, r.err, "worker %d hit a transport/decode error", i)
		if r.assigned {
			assigned++
			require.NotEmpty(t, r.resToken, "worker %d: assignment carries a reservation token", i)
			tokens[r.resToken]++
		}
	}
	// These consumers never settle. Register guaranteed cleanup so the
	// seeder's capacity is returned to the shared stack even if an assertion
	// below fails — otherwise the abandoned Load=cap would starve every
	// seeder-dependent scenario until the 30s TTL reaper runs.
	t.Cleanup(func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer rcancel()
		for tok := range tokens {
			if _, err := adminA().ForceFailInflight(rctx, tok); err != nil {
				t.Logf("e2e: scenario 28: force-fail %s: %v", tok, err)
			}
		}
	})
	assert.GreaterOrEqual(t, assigned, 1, "at least one consumer wins the seeder under concurrency")
	for tok, c := range tokens {
		assert.Equal(t, 1, c, "reservation token %s must be assigned to exactly one consumer (no double-assign)", tok)
	}
	// The seeder can hold at most LoadThreshold concurrent reservations;
	// the rest must be turned away, not over-committed. Default cap is 5.
	assert.LessOrEqual(t, assigned, 5, "no more than the seeder's load cap are assigned at once")
	t.Logf("e2e: scenario 28: %d/%d consumers assigned, %d turned away under a concurrent burst", assigned, n, n-assigned)

	// The tracker survived the burst: still healthy, chain still passes the
	// integrity gate, and no settlement completed (abandoned reservations),
	// so there are only STARTER_GRANT entries — no phantom USAGE debit.
	h, err := adminA().Health(ctx)
	require.NoError(t, err, "tracker-a healthy after the burst")
	require.Equal(t, "ok", h.Status)
	usageAfter := sqlCount(t, "tracker-a", "SELECT count(*) FROM entries WHERE kind=1;")
	assert.Equal(t, usageBefore, usageAfter, "an abandoned contention burst must not append any USAGE entry")

	// Operator recovery — the production remedy when consumers grab a seeder
	// and vanish. Force-failing each stuck in-flight request must return the
	// seeder's load slots immediately (the alternative is the 30s TTL reaper),
	// so the seeder's advertised capacity is fully restored for the next
	// consumer. Verify the load drains all the way back to zero.
	for tok := range tokens {
		_, err := adminA().ForceFailInflight(ctx, tok)
		require.NoError(t, err, "operator force-fail of abandoned assignment %s", tok)
	}
	sid, err := seederCtl().Identity(ctx)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		rec, err := adminA().Identity(ctx, sid.IdentityIDHex)
		return err == nil && rec.Seeder != nil && rec.Seeder.Load == 0
	}, 15*time.Second, 300*time.Millisecond, "seeder load reclaimed to zero after operator force-fail")
}

// TestScenario33_ConcurrentMixedConsumerBehaviors races consumers of DIFFERENT
// kinds at the same instant — the "all combinations of flows" case. Scenario
// 28 raced identical consumers; this proves the broker keeps each flow correct
// under concurrent HETEROGENEOUS demand: overspending consumers are rejected
// on balance (deterministically — the credit check precedes seeder selection,
// so it is independent of the concurrent load), well-funded consumers contend
// for the seeder's capped capacity, and the two classes never bleed into each
// other — an overspend never wins an assignment, and a funded request is never
// turned away with a credit verdict.
func TestScenario33_ConcurrentMixedConsumerBehaviors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	require.NoError(t, seederCtl().SetConfig(ctx, driver.SeederConfig{
		Available: true, Headroom: 0.9, Models: []string{sonnetModel}, Tiers: 1,
		SSEBody: cannedSSEBody,
	}), "configure the happy seeder")
	require.Eventually(t, func() bool {
		sid, err := seederCtl().Identity(ctx)
		if err != nil {
			return false
		}
		rec, err := adminA().Identity(ctx, sid.IdentityIDHex)
		return err == nil && rec.Seeder != nil && rec.Seeder.Available
	}, 20*time.Second, 500*time.Millisecond, "seeder becomes available")

	const overspendKind = "overspend"
	const fundedKind = "funded"
	// 6 funded (contend for the load cap of 5) + 5 overspend (all rejected).
	kinds := []string{}
	for i := 0; i < 6; i++ {
		kinds = append(kinds, fundedKind)
	}
	for i := 0; i < 5; i++ {
		kinds = append(kinds, overspendKind)
	}
	n := len(kinds)

	type outcome struct {
		kind     string
		assigned bool
		resToken string
		reason   string
		err      error
	}
	results := make([]outcome, n)

	var ready sync.WaitGroup
	ready.Add(n)
	gun := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(n)

	for i := 0; i < n; i++ {
		go func(i int, kind string) {
			defer wg.Done()
			res := outcome{kind: kind}
			cli := dialTrackerA(ctx, t)
			enrollClient(ctx, t, cli)
			snap, err := cli.VerifiedBalance(ctx, cli.IdentityID())
			if err != nil {
				res.err = err
				results[i] = res
				ready.Done()
				<-gun
				return
			}
			// funded: 5/5 → cost 90, well within the 1000 starter grant.
			// overspend: 400/0 → cost 1200, exceeding the whole grant.
			var maxIn, maxOut uint64 = 5, 5
			if kind == overspendKind {
				maxIn, maxOut = 400, 0
			}
			env := buildSignedBrokerEnvelope(t, cli, sonnetModel, maxIn, maxOut, snap)

			ready.Done()
			<-gun // fire together

			resp, err := cli.Call(ctx, tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST, env)
			if err != nil {
				res.err = err
				results[i] = res
				return
			}
			if resp.Status == tbproto.RpcStatus_RPC_STATUS_OK {
				var brr tbproto.BrokerRequestResponse
				if err := proto.Unmarshal(resp.Payload, &brr); err != nil {
					res.err = err
					results[i] = res
					return
				}
				if sa := brr.GetSeederAssignment(); sa != nil {
					res.assigned = true
					res.resToken = hex.EncodeToString(sa.ReservationToken)
				} else if nc := brr.GetNoCapacity(); nc != nil {
					res.reason = nc.GetReason()
				}
			}
			results[i] = res
		}(i, kinds[i])
	}

	ready.Wait()
	close(gun)
	wg.Wait()

	fundedAssigned := 0
	tokens := map[string]int{}
	for i, r := range results {
		require.NoError(t, r.err, "worker %d hit a transport/decode error", i)
		switch r.kind {
		case overspendKind:
			// The credit check precedes seeder selection, so an overspend is
			// ALWAYS rejected on balance regardless of the concurrent load.
			assert.False(t, r.assigned, "overspend worker %d must never win an assignment", i)
			assert.Equal(t, "insufficient_credits", r.reason,
				"overspend worker %d must be rejected on credits", i)
		case fundedKind:
			if r.assigned {
				fundedAssigned++
				require.NotEmpty(t, r.resToken, "funded worker %d assignment carries a token", i)
				tokens[r.resToken]++
			} else {
				// Turned away by contention — NEVER a credit verdict. This is
				// the cross-talk invariant: a funded consumer's outcome must
				// not be poisoned by the concurrent overspenders.
				assert.NotEqual(t, "insufficient_credits", r.reason,
					"funded worker %d turned away must be a capacity verdict, not a credit one", i)
			}
		}
	}
	assert.GreaterOrEqual(t, fundedAssigned, 1, "at least one funded consumer wins the seeder under concurrency")
	assert.LessOrEqual(t, fundedAssigned, 5, "no more than the seeder's load cap are assigned at once")
	for tok, c := range tokens {
		assert.Equalf(t, 1, c, "reservation token %s must be assigned to exactly one consumer", tok)
	}
	t.Logf("e2e: scenario 33: %d/6 funded assigned, %d funded turned away, 5/5 overspend rejected on credits",
		fundedAssigned, 6-fundedAssigned)

	h, err := adminA().Health(ctx)
	require.NoError(t, err, "tracker-a healthy after the mixed burst")
	require.Equal(t, "ok", h.Status)

	// Release the abandoned funded assignments' seeder load for the shared stack.
	t.Cleanup(func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer rcancel()
		for tok := range tokens {
			if _, err := adminA().ForceFailInflight(rctx, tok); err != nil {
				t.Logf("e2e: scenario 33: force-fail %s: %v", tok, err)
			}
		}
	})
}

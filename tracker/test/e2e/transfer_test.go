//go:build e2e

// Scenarios 14-16 of the plan's suite (Task 28): cross-region credit
// transfer (P5) between the two REAL trackers. The consumer actor —
// enrolled at tracker-a with its starter grant — drives POST /transfer,
// which sends a consumer-signed trackerclient.TransferRequest to
// tracker-B (the DESTINATION); tracker-b pulls a signed TRANSFER_PROOF
// from tracker-a over the live federation link; tracker-a commits the
// TRANSFER_OUT (kind=2) durably BEFORE returning the proof; tracker-b
// verifies the proof and commits the matching TRANSFER_IN (kind=3)
// under the same ref (= transfer nonce) before the RPC returns.
//
// # Cross-scenario state
//
// These scenarios share the one TestMain-managed stack with scenarios
// 1-13. Scenarios 3-7 (settlement_test.go) debit the consumer's
// tracker-a balance, so nothing here assumes the starter grant's 1000
// credits are intact: each scenario reads the consumer's CURRENT
// tracker-a balance first and sizes its transfer within it. Full-suite
// budget: scenario 14 moves <=50 and scenario 15 moves <=10 of the
// ~190 credits scenarios 3-7 leave behind; scenario 16 moves nothing
// (that's the point).
//
// # Scenario 15's idempotency scope (what IS and ISN'T drivable here)
//
// The plan's Task 28 Step 2 blurb says "replay the same transfer (same
// nonce) via a control toggle that reuses the nonce". No such toggle
// exists on the real consumer actor: plugin/cmd/tokenbay-e2e-actor/
// consumer.go's nextTransferNonce derives sha256(enroll_id || counter)
// from an in-memory counter that increments on EVERY POST /transfer, so
// a second /transfer is always a NEW transfer, never a wire replay of
// the previous nonce. A true same-nonce replay therefore cannot be
// driven end-to-end from the actor. What scenario 15 asserts instead is
// the DURABLE half of the idempotency design, on the real two-tracker
// ledgers: the on-chain per-kind single-use ref checks
// (ledger.ErrTransferRefExists; storage's idx_entries_ref_kind) —
// exactly one TRANSFER_OUT per ref at the source and exactly one
// TRANSFER_IN per ref at the destination, with balances moved exactly
// once. The in-memory replay layers those checks back (the source's
// issued-proof cache answering a re-pulled nonce, the destination's
// completed cache short-circuiting a replayed StartTransfer, and the
// dest-restart path where AppendTransferIn's ErrTransferRefExists is
// treated as idempotent success) are covered by the federation/ledger
// unit tests (tracker/internal/federation/transfer.go + transfer_test.go,
// tracker/internal/ledger/transfer_test.go), not re-driven here.
package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/tracker/test/e2e/driver"
)

// Single-integer sqlite queries for the transfer entry kinds
// (shared/proto EntryKind: TRANSFER_OUT=2 at the source, TRANSFER_IN=3
// at the destination). Executed via sqlCount (federation_test.go)
// against each tracker's /data/ledger.sqlite.
const (
	countTransferOutSQL = "SELECT count(*) FROM entries WHERE kind=2;"
	countTransferInSQL  = "SELECT count(*) FROM entries WHERE kind=3;"

	// Duplicate-ref probes: any ref appearing on more than one entry of
	// the same transfer kind would mean a double debit (source) or a
	// double credit (destination) — must always be 0.
	dupTransferOutRefsSQL = "SELECT count(*) FROM (SELECT ref FROM entries WHERE kind=2 GROUP BY ref HAVING count(*) > 1);"
	dupTransferInRefsSQL  = "SELECT count(*) FROM (SELECT ref FROM entries WHERE kind=3 GROUP BY ref HAVING count(*) > 1);"
)

// waitForTransferFederation gates every transfer scenario on BOTH
// trackers reporting a "steady" federation peer: the destination
// (tracker-b) can only pull the transfer proof over a live peer link to
// tracker-a, and earlier scenarios legitimately bounce that link
// (scenarios 8-10 restart tracker-a outright), so a fixed assumption
// would flake. Condition-polling, not a sleep.
func waitForTransferFederation(ctx context.Context, t *testing.T) {
	t.Helper()
	for _, tr := range []struct {
		name  string
		admin *driver.Admin
	}{
		{"tracker-a", adminA()},
		{"tracker-b", adminB()},
	} {
		eventually(t, 30*time.Second, 1*time.Second,
			tr.name+` /peers to show a peer in state "steady" (transfer needs the live a<->b link)`,
			func() bool {
				peers, err := tr.admin.Peers(ctx)
				if err != nil {
					return false
				}
				for _, p := range peers.Peers {
					if p.State == "steady" {
						return true
					}
				}
				return false
			})
	}
}

// consumerCreditsAtA reads the consumer's CURRENT tracker-a balance —
// never assuming the 1000-credit starter grant is intact (scenarios 3-7
// spend from it) — via the same admin poll bringup/settlement use.
func consumerCreditsAtA(ctx context.Context, t *testing.T, idHex string) int64 {
	t.Helper()
	return waitForBalance(ctx, t, idHex).Balance.Credits
}

// consumerCreditsAtB reads the consumer's tracker-b balance. Before the
// first transfer the consumer has never touched tracker-b's ledger (it
// isn't even enrolled there — P5 credits an unknown identity), so
// tracker-b's /identity/{id} legitimately 404s; that reads as 0, not an
// error. Any other failure fails the test.
func consumerCreditsAtB(ctx context.Context, t *testing.T, idHex string) int64 {
	t.Helper()
	id, err := adminB().Identity(ctx, idHex)
	if err != nil {
		var adminErr *driver.AdminError
		if errors.As(err, &adminErr) && adminErr.StatusCode == http.StatusNotFound {
			return 0
		}
		t.Fatalf("tracker-b /identity/%s: %v", idHex, err)
	}
	if id.Balance == nil {
		return 0
	}
	return id.Balance.Credits
}

// latestTransferRefHex returns the hex ref (= transfer nonce) of the
// newest entry of the given transfer kind in service's ledger, "" if
// none exists yet or the exec transiently fails (callers poll).
func latestTransferRefHex(t *testing.T, service string, kind int) string {
	t.Helper()
	out, err := compose().Exec(service, "sqlite3", ledgerDBPath,
		fmt.Sprintf("SELECT lower(hex(ref)) FROM entries WHERE kind=%d ORDER BY seq DESC LIMIT 1;", kind))
	if err != nil {
		t.Logf("e2e: latestTransferRefHex: %s exec error: %v", service, err)
		return ""
	}
	return strings.TrimSpace(out)
}

// transferAmountWithin picks a small transfer amount that is safely
// inside the consumer's current source balance: `want` when the balance
// covers it, the whole remaining balance otherwise (prior scenarios may
// have drained it — the scenario still proves the same protocol path
// with a tiny amount).
func transferAmountWithin(t *testing.T, balance, want int64) uint64 {
	t.Helper()
	require.GreaterOrEqual(t, balance, int64(1),
		"consumer needs at least 1 credit left at tracker-a to drive a transfer (earlier scenarios drained the starter grant completely?)")
	if balance < want {
		t.Logf("e2e: consumer balance at tracker-a is only %d, transferring that instead of the preferred %d", balance, want)
		return uint64(balance)
	}
	return uint64(want)
}

// TestScenario14_TransferHappyPath is scenario 14 (Task 28 Step 1): the
// first REAL two-tracker-over-network transfer. Drives the consumer's
// POST /transfer for N credits and asserts the full P5 contract: a
// synchronously returned source-signed proof (chain-tip hash + seq), a
// new TRANSFER_OUT (kind=2) on tracker-a and a new TRANSFER_IN (kind=3)
// on tracker-b bound by the SAME ref (nonce), and both balance
// projections moved by exactly N.
func TestScenario14_TransferHappyPath(t *testing.T) {
	ctx := context.Background()

	waitForTransferFederation(ctx, t)

	consumerID, err := consumerCtl().Identity(ctx)
	require.NoError(t, err, "consumer /identity")
	idHex := consumerID.IdentityIDHex

	balanceABefore := consumerCreditsAtA(ctx, t, idHex)
	balanceBBefore := consumerCreditsAtB(ctx, t, idHex)
	amount := transferAmountWithin(t, balanceABefore, 50)
	t.Logf("e2e: scenario 14: consumer=%s balanceA=%d balanceB=%d transferring %d", idHex, balanceABefore, balanceBBefore, amount)

	outBefore := sqlCount(t, "tracker-a", countTransferOutSQL)
	require.GreaterOrEqual(t, outBefore, 0, "tracker-a TRANSFER_OUT count readable before the transfer")
	inBefore := sqlCount(t, "tracker-b", countTransferInSQL)
	require.GreaterOrEqual(t, inBefore, 0, "tracker-b TRANSFER_IN count readable before the transfer")

	res, err := consumerCtl().Transfer(ctx, driver.TransferSpec{Amount: amount, DestRegion: "B"})
	require.NoError(t, err, "consumer POST /transfer")
	require.Empty(t, res.Error, "transfer should succeed end-to-end across the two real trackers")

	// The proof is returned synchronously: tracker-a signs (chain tip
	// hash, seq) over the committed TRANSFER_OUT before tracker-b ever
	// answers the consumer.
	assert.Len(t, res.SourceChainTipHashHex, 64, "source_chain_tip_hash should be 32 bytes of hex")
	assert.NotEqual(t, strings.Repeat("0", 64), res.SourceChainTipHashHex, "source chain tip hash should not be all-zero")
	assert.Greater(t, res.SourceSeq, uint64(0), "source_seq should point at the committed TRANSFER_OUT entry")

	// Source committed TRANSFER_OUT durably before returning the proof;
	// destination commits TRANSFER_IN before the RPC returns. Both
	// should already be visible — poll only to absorb docker-exec
	// transients, not product async-ness.
	eventually(t, 15*time.Second, 1*time.Second, "tracker-a to show one NEW TRANSFER_OUT (kind=2) entry", func() bool {
		return sqlCount(t, "tracker-a", countTransferOutSQL) == outBefore+1
	})
	eventually(t, 15*time.Second, 1*time.Second, "tracker-b to show one NEW TRANSFER_IN (kind=3) entry", func() bool {
		return sqlCount(t, "tracker-b", countTransferInSQL) == inBefore+1
	})

	// The two entries are the two halves of ONE transfer: same ref (=
	// the consumer's nonce) on both chains, and on the source side the
	// debited identity is the consumer.
	refOut := latestTransferRefHex(t, "tracker-a", 2)
	require.Len(t, refOut, 64, "tracker-a TRANSFER_OUT ref should be 32 bytes of hex")
	refIn := latestTransferRefHex(t, "tracker-b", 3)
	require.Len(t, refIn, 64, "tracker-b TRANSFER_IN ref should be 32 bytes of hex")
	assert.Equal(t, refOut, refIn, "TRANSFER_OUT@a and TRANSFER_IN@b must share the transfer nonce as ref")
	assert.Equal(t, 1, sqlCount(t, "tracker-a",
		fmt.Sprintf("SELECT count(*) FROM entries WHERE kind=2 AND ref=x'%s' AND lower(hex(consumer_id))='%s';", refOut, idHex)),
		"the new TRANSFER_OUT must debit the consumer's identity")

	// Balances moved by exactly N on both sides.
	eventually(t, 15*time.Second, 1*time.Second,
		fmt.Sprintf("consumer's tracker-a balance to drop to %d", balanceABefore-int64(amount)),
		func() bool {
			id, err := adminA().Identity(ctx, idHex)
			return err == nil && id.Balance != nil && id.Balance.Credits == balanceABefore-int64(amount)
		})
	eventually(t, 15*time.Second, 1*time.Second,
		fmt.Sprintf("consumer's tracker-b balance to rise to %d", balanceBBefore+int64(amount)),
		func() bool {
			return consumerCreditsAtB(ctx, t, idHex) == balanceBBefore+int64(amount)
		})
}

// TestScenario15_TransferIdempotency is scenario 15 (Task 28 Step 2):
// the durable no-double-apply guarantees around the transfer nonce. See
// the package doc comment for why this asserts the on-chain ref
// single-use invariant (per-ref uniqueness on BOTH real ledgers +
// exactly-once balance movement) rather than re-driving a same-nonce
// wire replay, which the real consumer actor cannot produce (its nonce
// counter advances on every /transfer).
func TestScenario15_TransferIdempotency(t *testing.T) {
	ctx := context.Background()

	waitForTransferFederation(ctx, t)

	consumerID, err := consumerCtl().Identity(ctx)
	require.NoError(t, err, "consumer /identity")
	idHex := consumerID.IdentityIDHex

	balanceABefore := consumerCreditsAtA(ctx, t, idHex)
	balanceBBefore := consumerCreditsAtB(ctx, t, idHex)
	amount := transferAmountWithin(t, balanceABefore, 10)

	inBefore := sqlCount(t, "tracker-b", countTransferInSQL)
	require.GreaterOrEqual(t, inBefore, 0, "tracker-b TRANSFER_IN count readable before the transfer")

	res, err := consumerCtl().Transfer(ctx, driver.TransferSpec{Amount: amount, DestRegion: "B"})
	require.NoError(t, err, "consumer POST /transfer")
	require.Empty(t, res.Error, "transfer should succeed before asserting its exactly-once accounting")

	eventually(t, 15*time.Second, 1*time.Second, "tracker-b to show this transfer's TRANSFER_IN entry", func() bool {
		return sqlCount(t, "tracker-b", countTransferInSQL) == inBefore+1
	})

	// This transfer's ref appears exactly once per side.
	refOut := latestTransferRefHex(t, "tracker-a", 2)
	require.Len(t, refOut, 64, "tracker-a TRANSFER_OUT ref should be 32 bytes of hex")
	assert.Equal(t, 1, sqlCount(t, "tracker-a",
		fmt.Sprintf("SELECT count(*) FROM entries WHERE kind=2 AND ref=x'%s';", refOut)),
		"exactly ONE TRANSFER_OUT for this nonce on tracker-a (single-use ref, double-debit defense)")
	assert.Equal(t, 1, sqlCount(t, "tracker-b",
		fmt.Sprintf("SELECT count(*) FROM entries WHERE kind=3 AND ref=x'%s';", refOut)),
		"exactly ONE TRANSFER_IN for this nonce on tracker-b (single-use ref, double-credit defense)")

	// And globally — across every transfer any scenario has driven on
	// this stack — no ref was ever applied twice on either chain.
	assert.Equal(t, 0, sqlCount(t, "tracker-a", dupTransferOutRefsSQL),
		"no transfer ref may ever have two TRANSFER_OUT entries on tracker-a")
	assert.Equal(t, 0, sqlCount(t, "tracker-b", dupTransferInRefsSQL),
		"no transfer ref may ever have two TRANSFER_IN entries on tracker-b")

	// Balance movement is exactly-once too: precisely N left A and
	// precisely N arrived at B, no double application anywhere.
	eventually(t, 15*time.Second, 1*time.Second, "balances to reflect exactly one application of the transfer", func() bool {
		id, err := adminA().Identity(ctx, idHex)
		if err != nil || id.Balance == nil {
			return false
		}
		return id.Balance.Credits == balanceABefore-int64(amount) &&
			consumerCreditsAtB(ctx, t, idHex) == balanceBBefore+int64(amount)
	})
}

// TestScenario16_TransferAuthz is scenario 16 (Task 28 Step 3, NEGATIVE
// test): a transfer for MORE than the consumer's current tracker-a
// balance must be rejected by the source-side balance guard
// (ledger.ErrInsufficientBalance — no balance may go below 0) with NO
// partial application: no new TRANSFER_OUT on tracker-a, no new
// TRANSFER_IN on tracker-b, and both balances unchanged. The rejection
// travels the full real path: tracker-a refuses the append, emits a
// signed TransferReject, tracker-b surfaces the reason to the
// consumer's pending TransferRequest RPC.
//
// KNOWN FAILURE — GENUINE PRODUCT BUG (found by this scenario's first
// live run; assertions deliberately NOT weakened, per this suite's
// prime directive; see .superpowers/sdd/task-28-report.md for the full
// evidence trail):
//
//	shared/federation/validate.go:29 — ValidateEnvelope caps the
//	envelope kind range at Kind_KIND_PEER_EXCHANGE (= 13), but
//	KIND_TRANSFER_REJECT (= 14, slice 13) and KIND_TRANSFER_REVERSAL
//	(= 15, slice 14) were added to the enum AFTER that bound was
//	written and it was never widened.
//
// Consequence, observed live on the real two-tracker stack: tracker-a's
// balance guard fires correctly (metric transfer_request_ledger_err=1,
// zero TRANSFER_OUT rows, balances untouched — the DURABLE half of this
// scenario is genuinely enforced), but the signed TransferReject can
// never leave the source — SendToPeer's SignEnvelope calls
// ValidateEnvelope, which rejects kind 14 ("kind 14 out of range"), so
// Send fails (metric transfer_reject_send_err=1) and (were it ever
// sent) the destination's recv path would drop it with the same
// validator. tracker-b's pending StartTransfer therefore hangs for the
// full 30s federation TransferTimeout and returns a generic INTERNAL
// error instead of the balance-guard reason; the consumer actor's own
// 10s /transfer budget (and this driver's 10s ctl HTTP timeout) expire
// first. The federation/ledger unit tests never caught it because they
// stub transferCoordinatorCfg.Send below the envelope layer. This test
// stays red until the validator bound is fixed (a one-line widening to
// KIND_TRANSFER_REVERSAL, plus the receive-side dispatch it unblocks).
func TestScenario16_TransferAuthz(t *testing.T) {
	ctx := context.Background()

	waitForTransferFederation(ctx, t)

	consumerID, err := consumerCtl().Identity(ctx)
	require.NoError(t, err, "consumer /identity")
	idHex := consumerID.IdentityIDHex

	balanceABefore := consumerCreditsAtA(ctx, t, idHex)
	balanceBBefore := consumerCreditsAtB(ctx, t, idHex)

	outBefore := sqlCount(t, "tracker-a", countTransferOutSQL)
	require.GreaterOrEqual(t, outBefore, 0, "tracker-a TRANSFER_OUT count readable before the over-balance attempt")
	inBefore := sqlCount(t, "tracker-b", countTransferInSQL)
	require.GreaterOrEqual(t, inBefore, 0, "tracker-b TRANSFER_IN count readable before the over-balance attempt")

	// Comfortably above the current balance, whatever earlier scenarios
	// left of it.
	excess := uint64(balanceABefore) + 1000
	res, err := consumerCtl().Transfer(ctx, driver.TransferSpec{Amount: excess, DestRegion: "B"})
	require.NoError(t, err, "consumer POST /transfer (the actor control call itself succeeds; the rejection rides in the result)")
	require.NotEmpty(t, res.Error,
		"an over-balance transfer (amount=%d > balance=%d) must be rejected, not applied", excess, balanceABefore)
	// The source's reject reason (ledger.ErrInsufficientBalance, capped
	// at 64 bytes by the TransferReject validator) is surfaced verbatim
	// through tracker-b to the consumer.
	assert.Contains(t, res.Error, "below permitted floor",
		"the rejection should carry the source ledger's balance-guard reason")
	assert.Empty(t, res.SourceChainTipHashHex, "no source proof may be issued for a rejected transfer")
	assert.Zero(t, res.SourceSeq, "no source seq may be issued for a rejected transfer")
	t.Logf("e2e: scenario 16: over-balance transfer rejected as expected: %s", res.Error)

	// No partial application anywhere. The source rejected BEFORE the
	// consumer got its error back, so these are deterministic reads, not
	// racy ones.
	assert.Equal(t, outBefore, sqlCount(t, "tracker-a", countTransferOutSQL),
		"a rejected transfer must not add a TRANSFER_OUT entry on tracker-a")
	assert.Equal(t, inBefore, sqlCount(t, "tracker-b", countTransferInSQL),
		"a rejected transfer must not add a TRANSFER_IN entry on tracker-b")

	balanceAAfter := consumerCreditsAtA(ctx, t, idHex)
	assert.Equal(t, balanceABefore, balanceAAfter, "consumer's tracker-a balance must be unchanged by the rejected transfer")
	balanceBAfter := consumerCreditsAtB(ctx, t, idHex)
	assert.Equal(t, balanceBBefore, balanceBAfter, "consumer's tracker-b balance must be unchanged by the rejected transfer")
}

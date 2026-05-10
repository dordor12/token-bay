package consumerflow

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/ratelimit"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)

// ---- helpers ---------------------------------------------------------------

// settlementTestBox bundles the coordinator + the fakes a settlement test
// most often needs to inspect. Keeps the setup line in tests short.
type settlementTestBox struct {
	c       *Coordinator
	broker  *fakeBroker
	audit   *recordingAudit
	ident   *fakeIdentity
	metrics *recordingSettlementMetrics
}

type recordingSettlementMetrics struct {
	mu       sync.Mutex
	outcomes []string
}

func (r *recordingSettlementMetrics) IncSettlementDecision(outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcomes = append(r.outcomes, outcome)
}

func (r *recordingSettlementMetrics) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.outcomes))
	copy(out, r.outcomes)
	return out
}

// newSettlementTestBox wires a Coordinator with deterministic deps and
// returns it alongside the most commonly inspected fakes. Identity uses a
// real ed25519 keypair so produced signatures verify against the public
// half — useful for assertions that the consumer signed the actual
// preimage_body bytes (not the hash).
func newSettlementTestBox(t *testing.T) *settlementTestBox {
	t.Helper()
	deps, broker, _, _, _, _, _, audit := newTestDeps(t)

	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	ident := &fakeIdentity{priv: priv}
	copy(ident.id[:], sha256.New().Sum(pub)[:32])
	deps.Identity = ident

	metrics := &recordingSettlementMetrics{}
	deps.Metrics = metrics

	c, err := New(deps)
	require.NoError(t, err)
	return &settlementTestBox{c: c, broker: broker, audit: audit, ident: ident, metrics: metrics}
}

// makeEntryBody builds a synthetic settlement preimage body for a given
// request_id with caller-supplied token counts.
func makeEntryBody(requestID [16]byte, model string, inputTokens, outputTokens uint32) *tbproto.EntryBody {
	return &tbproto.EntryBody{
		PrevHash:     bytesOfLen(32, 0x11),
		Seq:          42,
		Kind:         tbproto.EntryKind_ENTRY_KIND_USAGE,
		ConsumerId:   bytesOfLen(32, 0xAB),
		SeederId:     bytesOfLen(32, 0xEE),
		Model:        model,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		CostCredits:  1234,
		Timestamp:    1714000020,
		RequestId:    requestID[:],
	}
}

// makeSettlementRequest assembles a SettlementRequest with a self-consistent
// preimage hash + body so the handler's own SHA-256 check passes.
func makeSettlementRequest(t *testing.T, body *tbproto.EntryBody) *SettlementRequest {
	t.Helper()
	bodyBytes, err := signing.DeterministicMarshal(body)
	require.NoError(t, err)
	hash := sha256.Sum256(bodyBytes)
	return &SettlementRequest{PreimageHash: hash, PreimageBody: bodyBytes}
}

// reservationToken constructs the 16-byte reservation_token returned in a
// SeederAssignment from a request_id pattern fill byte.
func reservationToken(b byte) []byte {
	out := make([]byte, 16)
	for i := range out {
		out[i] = b
	}
	return out
}

// enterNetworkModeForTest drives OnStopFailure with a brokerResult that
// uses the given reservation_token (= request_id), returning the sessionID
// the test should reference.
func enterNetworkModeForTest(t *testing.T, c *Coordinator, broker *fakeBroker, sessionID string, reqIDFill byte) {
	t.Helper()
	broker.requestRes = &BrokerResult{
		Outcome: BrokerOutcomeAssignment,
		Assignment: &SeederAssignment{
			SeederAddr:       "127.0.0.1:9876",
			SeederPubkey:     bytesOfLen(32, 0xEE),
			ReservationToken: reservationToken(reqIDFill),
		},
	}
	require.NoError(t, c.OnStopFailure(context.Background(), &ratelimit.StopFailurePayload{
		SessionID:     sessionID,
		HookEventName: "StopFailure",
		Error:         ratelimit.ErrorRateLimit,
	}))
}

// ---- tests -----------------------------------------------------------------

func TestHandleSettlement_HappyPath_CallsSettleAndAudits(t *testing.T) {
	box := newSettlementTestBox(t)
	enterNetworkModeForTest(t, box.c, box.broker, "sess-A", 0xA1)

	var reqID [16]byte
	for i := range reqID {
		reqID[i] = 0xA1
	}
	body := makeEntryBody(reqID, "claude-sonnet-4-6", 100, 200)
	req := makeSettlementRequest(t, body)

	require.NoError(t, box.c.HandleSettlement(context.Background(), req))

	settles := box.broker.settleSnapshot()
	require.Len(t, settles, 1, "Settle RPC must be called exactly once")
	assert.Equal(t, req.PreimageHash[:], settles[0].preimageHash)
	// Sig must verify against the consumer's public key over the body bytes.
	pub := box.ident.priv.Public().(ed25519.PublicKey)
	require.True(t, ed25519.Verify(pub, req.PreimageBody, settles[0].sig),
		"countersig must be Ed25519 over preimage_body bytes (not the hash)")

	assert.Equal(t, []string{"countersigned"}, box.metrics.snapshot())

	records := box.audit.snapshot()
	require.GreaterOrEqual(t, len(records), 2, "want entry + settle audit records")
	settleRec := records[len(records)-1]
	assert.False(t, settleRec.ServedLocally, "settle audit is for a network-served request")
	assert.Equal(t, int64(1234), settleRec.CostCredits)
	assert.NotEmpty(t, settleRec.RequestID, "settle audit must reference a request_id")
}

func TestHandleSettlement_OverBudget_RefusesAndAudits(t *testing.T) {
	box := newSettlementTestBox(t)
	enterNetworkModeForTest(t, box.c, box.broker, "sess-B", 0xB2)

	var reqID [16]byte
	for i := range reqID {
		reqID[i] = 0xB2
	}
	// Default test deps have MaxOutputTokens=8192; report well over.
	body := makeEntryBody(reqID, "claude-sonnet-4-6", 100, 9_000)
	req := makeSettlementRequest(t, body)

	err := box.c.HandleSettlement(context.Background(), req)
	require.Error(t, err, "over-budget settlement must be refused")

	assert.Empty(t, box.broker.settleSnapshot(), "no Settle RPC on refusal")
	assert.Equal(t, []string{"refused_over_budget"}, box.metrics.snapshot())

	// Audit log records the refusal.
	records := box.audit.snapshot()
	require.NotEmpty(t, records)
	last := records[len(records)-1]
	assert.Contains(t, last.SeederID, "over_budget", "audit reason must explain the dispute")
}

func TestHandleSettlement_UnknownRequest_Refuses(t *testing.T) {
	box := newSettlementTestBox(t)
	// No network-mode entry — pendingSettlements is empty.

	var reqID [16]byte
	reqID[0] = 0xCC
	body := makeEntryBody(reqID, "claude-sonnet-4-6", 1, 1)
	req := makeSettlementRequest(t, body)

	err := box.c.HandleSettlement(context.Background(), req)
	require.Error(t, err)
	assert.Empty(t, box.broker.settleSnapshot())
	assert.Equal(t, []string{"refused_unknown_request"}, box.metrics.snapshot())

	records := box.audit.snapshot()
	require.Len(t, records, 1)
	assert.Contains(t, records[0].SeederID, "unknown_request")
}

func TestHandleSettlement_AfterExit_Refuses_Spec64(t *testing.T) {
	box := newSettlementTestBox(t)
	enterNetworkModeForTest(t, box.c, box.broker, "sess-D", 0xD4)

	// User exited network mode (spec §6.4 in-flight abort) before tracker
	// pushed settle_request.
	require.NoError(t, box.c.exitNetworkMode(context.Background(), "sess-D", "user_exit"))

	var reqID [16]byte
	for i := range reqID {
		reqID[i] = 0xD4
	}
	body := makeEntryBody(reqID, "claude-sonnet-4-6", 1, 1)
	req := makeSettlementRequest(t, body)

	err := box.c.HandleSettlement(context.Background(), req)
	require.Error(t, err)
	assert.Empty(t, box.broker.settleSnapshot(), "torn-down session must not be auto-countersigned")
}

func TestHandleSettlement_PreimageMismatch_Refuses(t *testing.T) {
	box := newSettlementTestBox(t)
	enterNetworkModeForTest(t, box.c, box.broker, "sess-E", 0xE5)

	var reqID [16]byte
	for i := range reqID {
		reqID[i] = 0xE5
	}
	body := makeEntryBody(reqID, "claude-sonnet-4-6", 1, 1)
	req := makeSettlementRequest(t, body)
	// Tamper: swap the hash so it no longer matches the body.
	req.PreimageHash[0] ^= 0xFF

	err := box.c.HandleSettlement(context.Background(), req)
	require.Error(t, err, "consumer must refuse to sign a body whose hash doesn't match")

	assert.Empty(t, box.broker.settleSnapshot())
	assert.Equal(t, []string{"refused_preimage_mismatch"}, box.metrics.snapshot())
	assert.Empty(t, box.ident.signedSnapshot(), "signer must not be invoked on mismatch")
}

func TestHandleSettlement_DecodeError_Refuses(t *testing.T) {
	box := newSettlementTestBox(t)

	garbage := bytes.Repeat([]byte{0xFF}, 16)
	hash := sha256.Sum256(garbage)
	req := &SettlementRequest{PreimageHash: hash, PreimageBody: garbage}

	err := box.c.HandleSettlement(context.Background(), req)
	require.Error(t, err)
	assert.Empty(t, box.broker.settleSnapshot())
	assert.Equal(t, []string{"refused_decode_error"}, box.metrics.snapshot())
}

func TestHandleSettlement_SignError_Refuses(t *testing.T) {
	box := newSettlementTestBox(t)
	enterNetworkModeForTest(t, box.c, box.broker, "sess-F", 0xF6)
	box.ident.signErr = errors.New("hsm offline")

	var reqID [16]byte
	for i := range reqID {
		reqID[i] = 0xF6
	}
	body := makeEntryBody(reqID, "claude-sonnet-4-6", 1, 1)
	req := makeSettlementRequest(t, body)

	err := box.c.HandleSettlement(context.Background(), req)
	require.Error(t, err)
	assert.Empty(t, box.broker.settleSnapshot())
	assert.Equal(t, []string{"refused_sign_error"}, box.metrics.snapshot())
}

func TestHandleSettlement_TrackerSettleError_Surfaces(t *testing.T) {
	box := newSettlementTestBox(t)
	enterNetworkModeForTest(t, box.c, box.broker, "sess-G", 0xC7)
	box.broker.settleErr = errors.New("tracker down")

	var reqID [16]byte
	for i := range reqID {
		reqID[i] = 0xC7
	}
	body := makeEntryBody(reqID, "claude-sonnet-4-6", 1, 1)
	req := makeSettlementRequest(t, body)

	err := box.c.HandleSettlement(context.Background(), req)
	require.Error(t, err, "tracker error must surface so the dispatcher suppresses the SettleAck")

	require.Len(t, box.broker.settleSnapshot(), 1, "we did try to call Settle")
	assert.Equal(t, []string{"refused_settle_error"}, box.metrics.snapshot())
}

func TestHandleSettlement_HappyPathThenDuplicateRefuses(t *testing.T) {
	box := newSettlementTestBox(t)
	enterNetworkModeForTest(t, box.c, box.broker, "sess-H", 0xC8)

	var reqID [16]byte
	for i := range reqID {
		reqID[i] = 0xC8
	}
	body := makeEntryBody(reqID, "claude-sonnet-4-6", 1, 1)
	req := makeSettlementRequest(t, body)

	require.NoError(t, box.c.HandleSettlement(context.Background(), req))
	require.Len(t, box.broker.settleSnapshot(), 1)

	// A second settle for the same request_id must refuse — the pending
	// entry was consumed.
	err := box.c.HandleSettlement(context.Background(), req)
	require.Error(t, err)
	assert.Len(t, box.broker.settleSnapshot(), 1, "duplicate must not double-settle")
}

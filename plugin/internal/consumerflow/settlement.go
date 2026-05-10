package consumerflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"

	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/plugin/internal/auditlog"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// Settlement decision outcomes — kept in lock-step with the contract
// documented on Deps.Metrics.IncSettlementDecision.
const (
	settleOutcomeOK               = "countersigned"
	settleOutcomeUnknownRequest   = "refused_unknown_request"
	settleOutcomeOverBudget       = "refused_over_budget"
	settleOutcomePreimageMismatch = "refused_preimage_mismatch"
	settleOutcomeDecodeError      = "refused_decode_error"
	settleOutcomeSignError        = "refused_sign_error"
	settleOutcomeSettleError      = "refused_settle_error"
)

// HandleSettlement is the consumer-side hook for tracker-pushed settlement
// requests. It verifies the preimage, looks up the pre-recorded reservation
// for the requested request_id, enforces the §11 token-budget ceiling, signs
// the body bytes via Identity.Sign, and pushes the signature back to the
// tracker via Tracker.Settle.
//
// Returns nil only on a successful Settle RPC. Every refusal path returns a
// non-nil error so the trackerclient push acceptor suppresses the SettleAck
// — the tracker treats the absence of an ack as a dispute (per spec).
func (c *Coordinator) HandleSettlement(ctx context.Context, req *SettlementRequest) error {
	if req == nil {
		return fmt.Errorf("consumerflow: nil settlement request")
	}

	// (1) Preimage hash must match SHA-256 of the body bytes — otherwise
	// the consumer would be signing a body whose hash differs from what
	// the tracker is recording on the chain.
	if sha256.Sum256(req.PreimageBody) != req.PreimageHash {
		c.recordSettleDecision(settleOutcomePreimageMismatch)
		c.auditSettleRefusal("", settleOutcomePreimageMismatch)
		return fmt.Errorf("consumerflow: preimage hash mismatch")
	}

	// (2) Decode the body so we can verify the request_id + token counts.
	var body tbproto.EntryBody
	if err := proto.Unmarshal(req.PreimageBody, &body); err != nil {
		c.recordSettleDecision(settleOutcomeDecodeError)
		c.auditSettleRefusal("", settleOutcomeDecodeError)
		return fmt.Errorf("consumerflow: decode entry body: %w", err)
	}

	// (3) The request_id must match a pending reservation for an active
	// fallback session. Settlement after exit, for an unknown request, or
	// a duplicate (entry already consumed) all land here.
	if len(body.RequestId) != 16 {
		c.recordSettleDecision(settleOutcomeUnknownRequest)
		c.auditSettleRefusal("", settleOutcomeUnknownRequest)
		return fmt.Errorf("consumerflow: settlement request_id length %d, want 16", len(body.RequestId))
	}
	var reqKey [16]byte
	copy(reqKey[:], body.RequestId)
	c.mu.Lock()
	pending, ok := c.pendingSettlements[reqKey]
	c.mu.Unlock()
	if !ok {
		c.recordSettleDecision(settleOutcomeUnknownRequest)
		c.auditSettleRefusal(hex.EncodeToString(reqKey[:]), settleOutcomeUnknownRequest)
		return fmt.Errorf("consumerflow: settlement for unknown request_id %x", reqKey)
	}

	// (4) §11 token-budget enforcement: the seeder cannot bill us for more
	// output tokens than the envelope advertised.
	if uint64(body.OutputTokens) > pending.maxOutputTokens {
		c.recordSettleDecision(settleOutcomeOverBudget)
		_ = c.deps.AuditLog.LogConsumer(auditlog.ConsumerRecord{
			RequestID:     "settle:" + hex.EncodeToString(reqKey[:]),
			ServedLocally: false,
			SeederID:      hex.EncodeToString(pending.seederPubkey) + ":" + settleOutcomeOverBudget,
			CostCredits:   clampToInt64(body.CostCredits),
			Timestamp:     c.deps.Now(),
		})
		return fmt.Errorf("consumerflow: settlement output_tokens=%d exceeds ceiling=%d", body.OutputTokens, pending.maxOutputTokens)
	}

	// (5) Sign the body bytes (not the hash). Verifiers reconstruct the
	// preimage from the body via DeterministicMarshal and verify against
	// our consumer pubkey — see ledger spec §3.1.
	sig, err := c.deps.Identity.Sign(req.PreimageBody)
	if err != nil {
		c.recordSettleDecision(settleOutcomeSignError)
		c.auditSettleRefusal(hex.EncodeToString(reqKey[:]), settleOutcomeSignError)
		return fmt.Errorf("consumerflow: sign settlement: %w", err)
	}

	// (6) Push the countersig to the tracker. The settle RPC takes the
	// 32-byte preimage hash + the consumer signature.
	if err := c.deps.Tracker.Settle(ctx, req.PreimageHash[:], sig); err != nil {
		c.recordSettleDecision(settleOutcomeSettleError)
		c.auditSettleRefusal(hex.EncodeToString(reqKey[:]), settleOutcomeSettleError)
		return fmt.Errorf("consumerflow: tracker Settle: %w", err)
	}

	// (7) Success — consume the pending entry so a duplicate push refuses,
	// emit the metric, and audit the settled cost.
	c.mu.Lock()
	delete(c.pendingSettlements, reqKey)
	c.mu.Unlock()
	c.recordSettleDecision(settleOutcomeOK)
	return c.deps.AuditLog.LogConsumer(auditlog.ConsumerRecord{
		RequestID:     "settle:" + hex.EncodeToString(reqKey[:]),
		ServedLocally: false,
		SeederID:      hex.EncodeToString(pending.seederPubkey),
		CostCredits:   clampToInt64(body.CostCredits),
		Timestamp:     c.deps.Now(),
	})
}

// clampToInt64 narrows a uint64 to int64 saturating at math.MaxInt64. The
// audit-log CostCredits field is int64 (auditlog.records.go), but the
// proto carries credits as uint64. Real settlement values are never
// remotely close to 2^63, so saturation is a safe over-cautious cap.
func clampToInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

// recordSettleDecision is nil-safe — Metrics is optional in Deps.
func (c *Coordinator) recordSettleDecision(outcome string) {
	if c.deps.Metrics == nil {
		return
	}
	c.deps.Metrics.IncSettlementDecision(outcome)
}

// auditSettleRefusal records a settlement refusal to the audit log. The
// SeederID field carries the human-readable reason (mirroring auditRefusal
// in coordinator.go); reqIDHex may be empty when refusal happens before the
// request_id is known (preimage mismatch, decode error).
func (c *Coordinator) auditSettleRefusal(reqIDHex, reason string) {
	if c.deps.AuditLog == nil {
		return
	}
	rid := "settle:refuse"
	if reqIDHex != "" {
		rid = "settle:refuse:" + reqIDHex
	}
	_ = c.deps.AuditLog.LogConsumer(auditlog.ConsumerRecord{
		RequestID:     rid,
		ServedLocally: false,
		SeederID:      reason,
		CostCredits:   0,
		Timestamp:     c.deps.Now(),
	})
}

package consumerflow

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"sync"
	"time"

	"github.com/token-bay/token-bay/plugin/internal/auditlog"
	"github.com/token-bay/token-bay/plugin/internal/ccproxy"
	"github.com/token-bay/token-bay/plugin/internal/envelopebuilder"
	"github.com/token-bay/token-bay/plugin/internal/exhaustionproofbuilder"
	"github.com/token-bay/token-bay/plugin/internal/hooks"
	"github.com/token-bay/token-bay/plugin/internal/ratelimit"
	"github.com/token-bay/token-bay/plugin/internal/settingsjson"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// defaultNetworkModeTTL matches plugin spec §5.5 (consumer.network_mode_ttl
// default 15 minutes).
const defaultNetworkModeTTL = 15 * time.Minute

// reapInterval is how often Run sweeps the active-sessions map for expired
// network-mode entries. Short relative to TTL; not user-tunable.
const reapInterval = 30 * time.Second

// bodyHashLen mirrors envelopebuilder.bodyHashLen. The v1 placeholder we
// fill at hook time is a fresh 32-byte random value — the request body is
// not available until ccproxy receives it.
const bodyHashLen = 32

// Coordinator is the consumer-side fallback orchestrator. It implements
// hooks.Sink so the dispatcher can feed events to it directly, and exposes
// Run for the sidecar supervisor to drive periodic state (TTL reap).
//
// Concurrency: Coordinator is safe for concurrent use; the active-session
// map is guarded by mu and each Sink method is synchronous w.r.t. its own
// session.
type Coordinator struct {
	deps Deps

	mu             sync.Mutex
	activeSessions map[string]*activeEntry
}

type activeEntry struct {
	enteredAt  time.Time
	expiresAt  time.Time
	sidecarURL string
}

// New validates deps and constructs the Coordinator. No goroutines start.
func New(deps Deps) (*Coordinator, error) {
	if err := deps.Validate(); err != nil {
		return nil, err
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.RandRead == nil {
		deps.RandRead = rand.Read
	}
	if deps.EphemeralKeyGen == nil {
		deps.EphemeralKeyGen = func() (ed25519.PublicKey, ed25519.PrivateKey, error) {
			return ed25519.GenerateKey(rand.Reader)
		}
	}
	if deps.NetworkModeTTL == 0 {
		deps.NetworkModeTTL = defaultNetworkModeTTL
	}
	if deps.PrivacyTier == tbproto.PrivacyTier_PRIVACY_TIER_UNSPECIFIED {
		deps.PrivacyTier = tbproto.PrivacyTier_PRIVACY_TIER_STANDARD
	}
	return &Coordinator{
		deps:           deps,
		activeSessions: make(map[string]*activeEntry),
	}, nil
}

// Run blocks until ctx is cancelled, periodically reaping expired
// network-mode entries. Safe to invoke as a goroutine.
func (c *Coordinator) Run(ctx context.Context) error {
	t := time.NewTicker(reapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			c.reapExpired(ctx)
		}
	}
}

// reapExpired sweeps for sessions whose TTL has elapsed and exits them.
func (c *Coordinator) reapExpired(ctx context.Context) {
	now := c.deps.Now()
	c.mu.Lock()
	expired := make([]string, 0)
	for sid, e := range c.activeSessions {
		if !e.expiresAt.IsZero() && now.After(e.expiresAt) {
			expired = append(expired, sid)
		}
	}
	c.mu.Unlock()
	for _, sid := range expired {
		_ = c.exitNetworkMode(ctx, sid, "ttl_expired")
	}
}

// OnStopFailure handles the StopFailure hook event. For matcher !=
// "rate_limit", returns nil immediately (per spec §2.1: only rate_limit
// is handled). For rate_limit, runs the §5.2 verification and §5.3
// network-mode entry sequence.
func (c *Coordinator) OnStopFailure(ctx context.Context, p *ratelimit.StopFailurePayload) error {
	if p == nil {
		return nil
	}
	if p.Error != ratelimit.ErrorRateLimit {
		// Other matchers (server_error, billing_error, etc.) are not
		// handled by Token-Bay per spec §2.1.
		return nil
	}
	return c.handleRateLimit(ctx, p, ratelimit.TriggerHook)
}

// OnSessionStart records the session for later activity tracking.
// Per spec §6.3 the seeder side cares about this; on the consumer side
// we use it primarily to know which sessions are live for cleanup.
func (c *Coordinator) OnSessionStart(_ context.Context, _ *hooks.SessionStartPayload) error {
	return nil
}

// OnSessionEnd exits network mode for the closing session, per spec §5.5.
func (c *Coordinator) OnSessionEnd(ctx context.Context, p *hooks.SessionEndPayload) error {
	if p == nil || p.SessionID == "" {
		return nil
	}
	c.mu.Lock()
	_, ok := c.activeSessions[p.SessionID]
	c.mu.Unlock()
	if !ok {
		return nil
	}
	return c.exitNetworkMode(ctx, p.SessionID, "session_end")
}

// OnUserPromptSubmit is reserved for a future pre-flight optimization
// (spec §2.1). v1 is a no-op.
func (c *Coordinator) OnUserPromptSubmit(_ context.Context, _ *hooks.UserPromptSubmitPayload) error {
	return nil
}

// handleRateLimit is the core §5.2/§5.3 path. Failure modes audit then
// return; the host Claude Code is never blocked by an internal error.
func (c *Coordinator) handleRateLimit(ctx context.Context, p *ratelimit.StopFailurePayload, trigger ratelimit.Trigger) error {
	if !c.deps.RuntimeProbeOK {
		// Spec §11: runtime probe failed → consumer fallback disabled.
		return c.auditRefusal(p.SessionID, "runtime_probe_failed")
	}

	sidecarURL := c.deps.SidecarURLFunc()
	if sidecarURL == "" {
		return c.auditRefusal(p.SessionID, "sidecar_url_unavailable")
	}

	// §11: settings.json incompatibility — Bedrock/Vertex/Foundry, JSONC
	// comments, pre-existing redirect, missing/non-writable file. Stage
	// 1 of the check is settingsjson.GetState; if Enter would refuse, we
	// refuse here too with a more specific reason.
	if err := c.preflightSettings(sidecarURL); err != nil {
		return c.auditRefusal(p.SessionID, "settings_"+errSlug(err))
	}

	// §5.2: invoke `claude /usage` PTY probe.
	usageBytes, err := c.deps.UsageProber.Probe(ctx)
	if err != nil {
		return c.auditRefusal(p.SessionID, "usage_probe_error")
	}
	usageVerdict := ratelimit.ParseUsageProbe(usageBytes)
	usageProbeAt := c.deps.Now()

	// Verdict matrix.
	gate := ratelimit.ApplyVerdictMatrix(trigger, p.Error, usageVerdict)
	if gate != ratelimit.VerdictProceed {
		return c.auditRefusal(p.SessionID, "gate_"+gate.String())
	}

	// §5.6: assemble exhaustion proof from cached payload + /usage bytes.
	stopFailureRaw, err := json.Marshal(p)
	if err != nil {
		return c.auditRefusal(p.SessionID, "marshal_stopfailure_error")
	}
	proof, err := c.deps.ProofBuilder.Build(exhaustionproofbuilder.ProofInput{
		StopFailureMatcher:    p.Error,
		StopFailureAt:         c.deps.Now(),
		StopFailureErrorShape: stopFailureRaw,
		UsageProbeAt:          usageProbeAt,
		UsageProbeOutput:      usageBytes,
	})
	if err != nil {
		return c.auditRefusal(p.SessionID, "proof_build_error")
	}

	// Fetch a fresh balance snapshot to attach to the envelope.
	balance, err := c.deps.Tracker.BalanceCached(ctx, c.deps.Identity.IdentityID())
	if err != nil {
		return c.auditRefusal(p.SessionID, "balance_fetch_error")
	}

	// §5.4: build envelope with v1 placeholder BodyHash. The actual
	// request body isn't available until ccproxy receives it; v1
	// pre-brokers a reservation token.
	bodyHash := make([]byte, bodyHashLen)
	if _, err := c.deps.RandRead(bodyHash); err != nil {
		return c.auditRefusal(p.SessionID, "rand_read_error")
	}
	envelope, err := c.deps.EnvelopeBuilder.Build(
		envelopebuilder.RequestSpec{
			Model:           c.deps.DefaultModel,
			MaxInputTokens:  c.deps.MaxInputTokens,
			MaxOutputTokens: c.deps.MaxOutputTokens,
			Tier:            c.deps.PrivacyTier,
			BodyHash:        bodyHash,
		},
		proof,
		balance,
	)
	if err != nil {
		return c.auditRefusal(p.SessionID, "envelope_build_error")
	}

	// §5.3 step 4: BrokerRequest the envelope.
	res, err := c.deps.Tracker.BrokerRequest(ctx, envelope)
	if err != nil {
		return c.auditRefusal(p.SessionID, "broker_request_error")
	}
	if res == nil || res.Outcome != BrokerOutcomeAssignment || res.Assignment == nil {
		return c.auditRefusal(p.SessionID, "broker_"+brokerOutcomeSlug(res))
	}

	// Generate consumer-side ephemeral keypair for the tunnel.
	_, ephemeralPriv, err := c.deps.EphemeralKeyGen()
	if err != nil {
		return c.auditRefusal(p.SessionID, "ephemeral_key_error")
	}

	seederAddr, err := netip.ParseAddrPort(res.Assignment.SeederAddr)
	if err != nil {
		return c.auditRefusal(p.SessionID, "bad_seeder_addr")
	}

	// Populate ccproxy.SessionModeStore — the redirected /v1/messages
	// arriving on ccproxy will look this up to dial the seeder.
	now := c.deps.Now()
	expiresAt := now.Add(c.deps.NetworkModeTTL)
	usageBytesCopy := append([]byte(nil), usageBytes...)
	c.deps.Sessions.EnterNetworkMode(p.SessionID, ccproxy.EntryMetadata{
		EnteredAt:          now,
		ExpiresAt:          expiresAt,
		StopFailurePayload: p,
		UsageProbeBytes:    usageBytesCopy,
		UsageVerdict:       usageVerdict,
		SeederAddr:         seederAddr,
		SeederPubkey:       ed25519.PublicKey(append([]byte(nil), res.Assignment.SeederPubkey...)),
		EphemeralPriv:      ephemeralPriv,
	})

	// §5.3 step 4: atomically rewrite ~/.claude/settings.json.
	if err := c.deps.Settings.EnterNetworkMode(sidecarURL, p.SessionID); err != nil {
		// Roll back the session-store entry — settings.json failed to
		// pivot, so the redirected request would never arrive anyway.
		c.deps.Sessions.ExitNetworkMode(p.SessionID)
		return c.auditRefusal(p.SessionID, "settings_"+errSlug(err))
	}

	c.mu.Lock()
	c.activeSessions[p.SessionID] = &activeEntry{
		enteredAt:  now,
		expiresAt:  expiresAt,
		sidecarURL: sidecarURL,
	}
	c.mu.Unlock()

	// Audit the successful entry. v1 records ServedLocally=false to denote
	// "fallback path engaged"; cost is unknown until settlement.
	return c.deps.AuditLog.LogConsumer(auditlog.ConsumerRecord{
		RequestID:     fallbackRequestID(envelope),
		ServedLocally: false,
		SeederID:      hex.EncodeToString(res.Assignment.SeederPubkey),
		CostCredits:   0,
		Timestamp:     now,
	})
}

// preflightSettings consults settingsjson.GetState to surface §11 refusals
// before we burn an envelope and broker round-trip on a redirect we can't
// possibly write.
func (c *Coordinator) preflightSettings(sidecarURL string) error {
	state, err := c.deps.Settings.GetState(sidecarURL)
	if err != nil {
		return err
	}
	if state.BedrockEnabled {
		return settingsjson.ErrBedrockProvider
	}
	if state.VertexEnabled {
		return settingsjson.ErrVertexProvider
	}
	if state.FoundryEnabled {
		return settingsjson.ErrFoundryProvider
	}
	if state.HasJSONCComments {
		return settingsjson.ErrIncompatibleJSONCComments
	}
	if state.ExistingBaseURL != "" && !state.ExistingBaseURLMatches {
		return settingsjson.ErrPreExistingRedirect
	}
	return nil
}

// exitNetworkMode reverses the §5.3 entry: deletes the SessionModeStore
// entry, rewrites settings.json via the rollback journal, and clears the
// local activeSessions record. Surfaces the §5.5 Object.assign caveat in
// the audit-log reason so the cmd layer / UI can show the warning.
func (c *Coordinator) exitNetworkMode(_ context.Context, sessionID, reason string) error {
	c.deps.Sessions.ExitNetworkMode(sessionID)

	c.mu.Lock()
	entry, ok := c.activeSessions[sessionID]
	if ok {
		delete(c.activeSessions, sessionID)
	}
	c.mu.Unlock()

	var settingsErr error
	if ok {
		// Prefer the journal-based exit for a clean restore; fall back to
		// best-effort clearing if the journal is missing.
		settingsErr = c.deps.Settings.ExitNetworkMode()
		if settingsErr != nil {
			settingsErr = c.deps.Settings.ExitNetworkModeBestEffort(entry.sidecarURL)
		}
	}

	if logErr := c.deps.AuditLog.LogConsumer(auditlog.ConsumerRecord{
		RequestID:     "exit:" + sessionID,
		ServedLocally: true,
		CostCredits:   0,
		Timestamp:     c.deps.Now(),
		SeederID:      reason,
	}); logErr != nil && settingsErr == nil {
		return logErr
	}
	return settingsErr
}

// auditRefusal records that a fallback was rejected before network-mode
// entry. The host Claude Code is not blocked. Returns nil so the hook
// dispatcher's empty-Response wire contract holds.
func (c *Coordinator) auditRefusal(sessionID, reason string) error {
	if c.deps.AuditLog == nil {
		return nil
	}
	_ = c.deps.AuditLog.LogConsumer(auditlog.ConsumerRecord{
		RequestID:     "refuse:" + sessionID,
		ServedLocally: true,
		SeederID:      reason,
		CostCredits:   0,
		Timestamp:     c.deps.Now(),
	})
	return nil
}

// errSlug returns a stable short identifier for an error chain. Used to
// classify settings-related refusals in audit logs.
func errSlug(err error) string {
	switch {
	case errors.Is(err, settingsjson.ErrBedrockProvider):
		return "bedrock"
	case errors.Is(err, settingsjson.ErrVertexProvider):
		return "vertex"
	case errors.Is(err, settingsjson.ErrFoundryProvider):
		return "foundry"
	case errors.Is(err, settingsjson.ErrIncompatibleJSONCComments):
		return "jsonc_comments"
	case errors.Is(err, settingsjson.ErrPreExistingRedirect):
		return "preexisting_redirect"
	case errors.Is(err, settingsjson.ErrSettingsNotWritable):
		return "not_writable"
	case errors.Is(err, settingsjson.ErrSymlinkEscapesClaudeDir):
		return "symlink_escape"
	default:
		return "error"
	}
}

func brokerOutcomeSlug(res *BrokerResult) string {
	if res == nil {
		return "nil_response"
	}
	switch res.Outcome {
	case BrokerOutcomeNoCapacity:
		return "no_capacity"
	case BrokerOutcomeQueued:
		return "queued"
	case BrokerOutcomeRejected:
		return "rejected"
	default:
		return "unknown"
	}
}

// fallbackRequestID returns a stable identifier per fallback envelope.
// Uses the envelope's nonce since BodyHash is a placeholder pre-broker.
func fallbackRequestID(env *tbproto.EnvelopeSigned) string {
	if env == nil || env.Body == nil {
		return ""
	}
	return "fb:" + hex.EncodeToString(env.Body.Nonce)
}

// Compile-time assertion that *Coordinator satisfies hooks.Sink.
var _ hooks.Sink = (*Coordinator)(nil)

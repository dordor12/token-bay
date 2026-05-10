package consumerflow

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/auditlog"
	"github.com/token-bay/token-bay/plugin/internal/ccproxy"
	"github.com/token-bay/token-bay/plugin/internal/envelopebuilder"
	"github.com/token-bay/token-bay/plugin/internal/exhaustionproofbuilder"
	"github.com/token-bay/token-bay/plugin/internal/hooks"
	"github.com/token-bay/token-bay/plugin/internal/ratelimit"
	"github.com/token-bay/token-bay/plugin/internal/settingsjson"
	"github.com/token-bay/token-bay/shared/exhaustionproof"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// ---- fakes -----------------------------------------------------------------

type fakeBroker struct {
	mu          sync.Mutex
	requests    []*tbproto.EnvelopeSigned
	requestRes  *BrokerResult
	requestErr  error
	balanceCall int
	balance     *tbproto.SignedBalanceSnapshot
	balanceErr  error
	settles     []settleCall
	settleErr   error
}

type settleCall struct {
	preimageHash []byte
	sig          []byte
}

func (f *fakeBroker) BrokerRequest(_ context.Context, env *tbproto.EnvelopeSigned) (*BrokerResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, env)
	return f.requestRes, f.requestErr
}

func (f *fakeBroker) BalanceCached(_ context.Context, _ ids.IdentityID) (*tbproto.SignedBalanceSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.balanceCall++
	return f.balance, f.balanceErr
}

func (f *fakeBroker) Settle(_ context.Context, preimageHash, sig []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settles = append(f.settles, settleCall{
		preimageHash: append([]byte(nil), preimageHash...),
		sig:          append([]byte(nil), sig...),
	})
	return f.settleErr
}

func (f *fakeBroker) settleSnapshot() []settleCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]settleCall, len(f.settles))
	copy(out, f.settles)
	return out
}

type fakeProber struct {
	mu       sync.Mutex
	calls    int
	out      []byte
	err      error
	blockReq chan struct{}
}

func (f *fakeProber) Probe(ctx context.Context) ([]byte, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.blockReq != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.blockReq:
		}
	}
	return f.out, f.err
}

func (f *fakeProber) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type fakeProofBuilder struct {
	mu     sync.Mutex
	inputs []exhaustionproofbuilder.ProofInput
	out    *exhaustionproof.ExhaustionProofV1
	err    error
}

func (f *fakeProofBuilder) Build(in exhaustionproofbuilder.ProofInput) (*exhaustionproof.ExhaustionProofV1, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, in)
	if f.err != nil {
		return nil, f.err
	}
	return f.out, nil
}

type fakeEnvelopeBuilder struct {
	mu     sync.Mutex
	specs  []envelopebuilder.RequestSpec
	out    *tbproto.EnvelopeSigned
	err    error
	probes []*exhaustionproof.ExhaustionProofV1
}

func (f *fakeEnvelopeBuilder) Build(spec envelopebuilder.RequestSpec, p *exhaustionproof.ExhaustionProofV1, _ *tbproto.SignedBalanceSnapshot) (*tbproto.EnvelopeSigned, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs = append(f.specs, spec)
	f.probes = append(f.probes, p)
	if f.err != nil {
		return nil, f.err
	}
	if f.out != nil {
		return f.out, nil
	}
	// default: a minimally-shaped envelope
	return &tbproto.EnvelopeSigned{
		Body: &tbproto.EnvelopeBody{
			ProtocolVersion: uint32(tbproto.ProtocolVersion),
			Model:           spec.Model,
			BodyHash:        spec.BodyHash,
			Nonce:           bytesOfLen(16, 0xCD),
			ConsumerId:      bytesOfLen(32, 0xAB),
		},
		ConsumerSig: bytesOfLen(64, 0x55),
	}, nil
}

type fakeIdentity struct {
	id      ids.IdentityID
	priv    ed25519.PrivateKey
	signErr error

	mu     sync.Mutex
	signed [][]byte
}

func (f *fakeIdentity) IdentityID() ids.IdentityID { return f.id }

func (f *fakeIdentity) Sign(msg []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signed = append(f.signed, append([]byte(nil), msg...))
	if f.signErr != nil {
		return nil, f.signErr
	}
	if f.priv == nil {
		// Default: deterministic 64-byte fake signature derived from the
		// first 8 bytes of msg, suitable for assert.Equal comparisons in
		// tests that don't care about cryptographic validity.
		out := make([]byte, ed25519.SignatureSize)
		copy(out, msg)
		return out, nil
	}
	return ed25519.Sign(f.priv, msg), nil
}

func (f *fakeIdentity) signedSnapshot() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.signed))
	copy(out, f.signed)
	return out
}

// recordingAudit is a test-only AuditWriter that captures records in
// memory so tests can assert on refusal reasons without parsing log files.
type recordingAudit struct {
	mu      sync.Mutex
	records []auditlog.ConsumerRecord
}

func (r *recordingAudit) LogConsumer(rec auditlog.ConsumerRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec)
	return nil
}

func (r *recordingAudit) snapshot() []auditlog.ConsumerRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]auditlog.ConsumerRecord, len(r.records))
	copy(out, r.records)
	return out
}

// ---- helpers ---------------------------------------------------------------

func bytesOfLen(n int, fill byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = fill
	}
	return out
}

func newTestStore(t *testing.T) *settingsjson.Store {
	t.Helper()
	dir := t.TempDir()
	return settingsjson.NewStoreAt(
		filepath.Join(dir, "claude", "settings.json"),
		filepath.Join(dir, "tokenbay", "settings-rollback.json"),
	)
}

func validProof() *exhaustionproof.ExhaustionProofV1 {
	return &exhaustionproof.ExhaustionProofV1{
		StopFailure: &exhaustionproof.StopFailure{
			Matcher:    "rate_limit",
			At:         1714000000,
			ErrorShape: []byte(`{"type":"rate_limit_error"}`),
		},
		UsageProbe: &exhaustionproof.UsageProbe{
			At:     1714000005,
			Output: []byte(`Current session: 99% used`),
		},
		CapturedAt: 1714000010,
		Nonce:      bytesOfLen(16, 0xCC),
	}
}

func goodAssignmentResult() *BrokerResult {
	return &BrokerResult{
		Outcome: BrokerOutcomeAssignment,
		Assignment: &SeederAssignment{
			SeederAddr:       "127.0.0.1:9876",
			SeederPubkey:     bytesOfLen(32, 0xEE),
			ReservationToken: bytesOfLen(16, 0xF0),
		},
	}
}

func newTestDeps(t *testing.T) (Deps, *fakeBroker, *fakeProber, *fakeProofBuilder, *fakeEnvelopeBuilder, *ccproxy.SessionModeStore, *settingsjson.Store, *recordingAudit) { //nolint:unparam // test helper signature
	t.Helper()
	store := newTestStore(t)

	broker := &fakeBroker{requestRes: goodAssignmentResult(), balance: &tbproto.SignedBalanceSnapshot{Body: &tbproto.BalanceSnapshotBody{Credits: 100}, TrackerSig: bytesOfLen(64, 0x77)}}
	prober := &fakeProber{out: []byte("Current session: 99% used\nCurrent week: 99% used\n")}
	proofB := &fakeProofBuilder{out: validProof()}
	envB := &fakeEnvelopeBuilder{}
	sessionStore := ccproxy.NewSessionModeStore()
	audit := &recordingAudit{}
	id := ids.IdentityID{}
	for i := range id {
		id[i] = byte(i + 1)
	}

	deps := Deps{
		Tracker:         broker,
		Sessions:        sessionStore,
		Settings:        store,
		AuditLog:        audit,
		UsageProber:     prober,
		ProofBuilder:    proofB,
		EnvelopeBuilder: envB,
		Identity:        &fakeIdentity{id: id},
		SidecarURLFunc:  func() string { return "http://127.0.0.1:54321/" },
		DefaultModel:    "claude-sonnet-4-6",
		MaxInputTokens:  200_000,
		MaxOutputTokens: 8_192,
		PrivacyTier:     tbproto.PrivacyTier_PRIVACY_TIER_STANDARD,
		RuntimeProbeOK:  true,
		// Use real time so SessionModeStore.GetMode (which compares
		// against time.Now()) treats freshly-entered sessions as live.
		Now: time.Now,
		EphemeralKeyGen: func() (ed25519.PublicKey, ed25519.PrivateKey, error) {
			pub, priv, err := ed25519.GenerateKey(nil)
			return pub, priv, err
		},
		RandRead: func(p []byte) (int, error) {
			for i := range p {
				p[i] = 0xAB
			}
			return len(p), nil
		},
	}
	return deps, broker, prober, proofB, envB, sessionStore, store, audit
}

// ---- tests -----------------------------------------------------------------

func TestNew_RejectsEmptyDeps(t *testing.T) {
	_, err := New(Deps{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidDeps)
}

func TestNew_AcceptsValidDeps(t *testing.T) {
	deps, _, _, _, _, _, _, _ := newTestDeps(t)
	c, err := New(deps)
	require.NoError(t, err)
	require.NotNil(t, c)
}

func TestOnStopFailure_NonRateLimit_NoOp(t *testing.T) {
	deps, broker, prober, _, _, sessionStore, _, _ := newTestDeps(t)
	c, err := New(deps)
	require.NoError(t, err)

	err = c.OnStopFailure(context.Background(), &ratelimit.StopFailurePayload{
		SessionID:     "sess-A",
		HookEventName: "StopFailure",
		Error:         ratelimit.ErrorServerError,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, prober.callCount(), "non-rate_limit error must not invoke /usage probe")
	assert.Empty(t, broker.requests, "non-rate_limit error must not call BrokerRequest")
	mode, _ := sessionStore.GetMode("sess-A")
	assert.Equal(t, ccproxy.ModePassThrough, mode)
}

func TestOnStopFailure_RateLimit_HappyPath(t *testing.T) {
	deps, broker, prober, proofB, envB, sessionStore, settingsStore, audit := newTestDeps(t)
	c, err := New(deps)
	require.NoError(t, err)

	payload := &ratelimit.StopFailurePayload{
		SessionID:     "sess-happy",
		HookEventName: "StopFailure",
		Error:         ratelimit.ErrorRateLimit,
	}
	err = c.OnStopFailure(context.Background(), payload)
	require.NoError(t, err)

	// Audit log records the successful entry (one record, ServedLocally=false).
	records := audit.snapshot()
	require.Len(t, records, 1)
	assert.False(t, records[0].ServedLocally)

	// (a) /usage probe ran
	assert.Equal(t, 1, prober.callCount())

	// (b) proof + envelope were built
	require.Len(t, proofB.inputs, 1)
	assert.Equal(t, "rate_limit", proofB.inputs[0].StopFailureMatcher)
	assert.NotEmpty(t, proofB.inputs[0].StopFailureErrorShape)
	assert.NotEmpty(t, proofB.inputs[0].UsageProbeOutput)

	require.Len(t, envB.specs, 1)
	assert.Equal(t, "claude-sonnet-4-6", envB.specs[0].Model)
	assert.Equal(t, uint64(200_000), envB.specs[0].MaxInputTokens)
	assert.Equal(t, uint64(8_192), envB.specs[0].MaxOutputTokens)
	assert.Equal(t, tbproto.PrivacyTier_PRIVACY_TIER_STANDARD, envB.specs[0].Tier)
	assert.Len(t, envB.specs[0].BodyHash, 32)

	// (c) BrokerRequest received the envelope
	require.Len(t, broker.requests, 1)
	assert.NotNil(t, broker.requests[0])

	// (d) SessionModeStore is in network mode for sess-happy
	mode, meta := sessionStore.GetMode("sess-happy")
	assert.Equal(t, ccproxy.ModeNetwork, mode)
	require.NotNil(t, meta)
	assert.Equal(t, "127.0.0.1:9876", meta.SeederAddr.String())
	assert.Equal(t, ed25519.PublicKey(bytesOfLen(32, 0xEE)), meta.SeederPubkey)
	assert.Len(t, meta.EphemeralPriv, ed25519.PrivateKeySize)

	// (e) settings.json was rewritten — verify via re-reading state.
	state, err := settingsStore.GetState("http://127.0.0.1:54321/")
	require.NoError(t, err)
	assert.True(t, state.SettingsFileExists)
	assert.True(t, state.ExistingBaseURLMatches)
	assert.True(t, state.InNetworkMode)
}

func TestOnStopFailure_RuntimeProbeFailed_Refuses(t *testing.T) {
	deps, broker, prober, _, _, sessionStore, settingsStore, _ := newTestDeps(t)
	deps.RuntimeProbeOK = false
	c, err := New(deps)
	require.NoError(t, err)

	err = c.OnStopFailure(context.Background(), &ratelimit.StopFailurePayload{
		SessionID:     "sess-noprobe",
		HookEventName: "StopFailure",
		Error:         ratelimit.ErrorRateLimit,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, prober.callCount(), "must not probe when RuntimeProbeOK=false")
	assert.Empty(t, broker.requests)
	mode, _ := sessionStore.GetMode("sess-noprobe")
	assert.Equal(t, ccproxy.ModePassThrough, mode)

	state, err := settingsStore.GetState("http://127.0.0.1:54321/")
	require.NoError(t, err)
	assert.False(t, state.InNetworkMode)
}

func TestOnStopFailure_BedrockProvider_Refuses(t *testing.T) {
	deps, broker, prober, _, _, sessionStore, store, _ := newTestDeps(t)
	// Pre-write a settings.json with CLAUDE_CODE_USE_BEDROCK truthy.
	require.NoError(t, writeSettings(t, store.SettingsPath, `{"env":{"CLAUDE_CODE_USE_BEDROCK":"1"}}`))
	c, err := New(deps)
	require.NoError(t, err)

	err = c.OnStopFailure(context.Background(), &ratelimit.StopFailurePayload{
		SessionID:     "sess-bedrock",
		HookEventName: "StopFailure",
		Error:         ratelimit.ErrorRateLimit,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, prober.callCount(), "must not probe when settings.json refuses")
	assert.Empty(t, broker.requests)
	mode, _ := sessionStore.GetMode("sess-bedrock")
	assert.Equal(t, ccproxy.ModePassThrough, mode)
}

func TestOnStopFailure_UsageHeadroom_Refuses(t *testing.T) {
	deps, broker, prober, _, _, sessionStore, _, _ := newTestDeps(t)
	prober.out = []byte(`Current session: 10% used\nCurrent week (all models): 10% used\nCurrent week (Sonnet): 5% used`)
	c, err := New(deps)
	require.NoError(t, err)

	err = c.OnStopFailure(context.Background(), &ratelimit.StopFailurePayload{
		SessionID:     "sess-headroom",
		HookEventName: "StopFailure",
		Error:         ratelimit.ErrorRateLimit,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, prober.callCount(), "probe must run before verdict")
	assert.Empty(t, broker.requests, "headroom verdict must not broker the request")
	mode, _ := sessionStore.GetMode("sess-headroom")
	assert.Equal(t, ccproxy.ModePassThrough, mode)
}

func TestOnStopFailure_BrokerNoCapacity_RefusesAndDoesNotEnterMode(t *testing.T) {
	deps, broker, _, _, _, sessionStore, settingsStore, _ := newTestDeps(t)
	broker.requestRes = &BrokerResult{
		Outcome: BrokerOutcomeNoCapacity,
		NoCap:   &NoCapacityResult{Reason: "no seeders"},
	}
	c, err := New(deps)
	require.NoError(t, err)

	err = c.OnStopFailure(context.Background(), &ratelimit.StopFailurePayload{
		SessionID:     "sess-nocap",
		HookEventName: "StopFailure",
		Error:         ratelimit.ErrorRateLimit,
	})
	require.NoError(t, err)
	require.Len(t, broker.requests, 1)
	mode, _ := sessionStore.GetMode("sess-nocap")
	assert.Equal(t, ccproxy.ModePassThrough, mode, "no_capacity must not enter network mode")

	state, err := settingsStore.GetState("http://127.0.0.1:54321/")
	require.NoError(t, err)
	assert.False(t, state.InNetworkMode, "settings.json must not be rewritten on broker no_capacity")
}

func TestOnStopFailure_BrokerError_AuditsAndRollsBack(t *testing.T) {
	deps, broker, _, _, _, sessionStore, settingsStore, _ := newTestDeps(t)
	broker.requestErr = errors.New("tracker down")
	c, err := New(deps)
	require.NoError(t, err)

	err = c.OnStopFailure(context.Background(), &ratelimit.StopFailurePayload{
		SessionID:     "sess-err",
		HookEventName: "StopFailure",
		Error:         ratelimit.ErrorRateLimit,
	})
	require.NoError(t, err, "broker error must not bubble to caller — host Claude Code stays unblocked")

	mode, _ := sessionStore.GetMode("sess-err")
	assert.Equal(t, ccproxy.ModePassThrough, mode)

	state, err := settingsStore.GetState("http://127.0.0.1:54321/")
	require.NoError(t, err)
	assert.False(t, state.InNetworkMode)
}

func TestOnSessionEnd_ExitsNetworkMode(t *testing.T) {
	deps, _, _, _, _, sessionStore, settingsStore, _ := newTestDeps(t)
	c, err := New(deps)
	require.NoError(t, err)

	// First, enter network mode for sess-bye.
	err = c.OnStopFailure(context.Background(), &ratelimit.StopFailurePayload{
		SessionID:     "sess-bye",
		HookEventName: "StopFailure",
		Error:         ratelimit.ErrorRateLimit,
	})
	require.NoError(t, err)
	mode, _ := sessionStore.GetMode("sess-bye")
	require.Equal(t, ccproxy.ModeNetwork, mode)

	// Now SessionEnd → exit.
	err = c.OnSessionEnd(context.Background(), &hooks.SessionEndPayload{
		SessionID:     "sess-bye",
		HookEventName: "SessionEnd",
		Reason:        "logout",
	})
	require.NoError(t, err)
	mode, _ = sessionStore.GetMode("sess-bye")
	assert.Equal(t, ccproxy.ModePassThrough, mode)

	state, err := settingsStore.GetState("http://127.0.0.1:54321/")
	require.NoError(t, err)
	assert.False(t, state.InNetworkMode, "exit must clear settings.json redirect")
}

func TestOnSessionEnd_UnknownSession_NoOp(t *testing.T) {
	deps, _, _, _, _, _, _, _ := newTestDeps(t)
	c, err := New(deps)
	require.NoError(t, err)

	err = c.OnSessionEnd(context.Background(), &hooks.SessionEndPayload{
		SessionID:     "never-entered",
		HookEventName: "SessionEnd",
		Reason:        "clear",
	})
	require.NoError(t, err)
}

// TestDispatcherIntegration drives a StopFailure JSON payload through
// hooks.Dispatcher → Coordinator and verifies the same end-to-end
// invariants as TestOnStopFailure_RateLimit_HappyPath. This is the
// acceptance test from the task brief.
func TestDispatcherIntegration_StopFailure_RateLimit(t *testing.T) {
	deps, broker, prober, _, envB, sessionStore, settingsStore, _ := newTestDeps(t)
	c, err := New(deps)
	require.NoError(t, err)

	d := &hooks.Dispatcher{Sink: c}

	payload := []byte(`{
		"session_id":      "sess-disp",
		"transcript_path": "/tmp/x",
		"cwd":             "/tmp",
		"hook_event_name": "StopFailure",
		"error":           "rate_limit"
	}`)

	var out bytes.Buffer
	err = d.Handle(context.Background(), hooks.EventNameStopFailure, bytes.NewReader(payload), &out)
	require.NoError(t, err)

	// Dispatcher always emits an empty-Response object on the wire.
	assert.NotEmpty(t, out.Bytes())

	// (a) probe ran, (b) envelope built, (c) broker called, (d) network mode, (e) settings rewritten.
	assert.Equal(t, 1, prober.callCount())
	assert.Len(t, envB.specs, 1)
	require.Len(t, broker.requests, 1)
	mode, _ := sessionStore.GetMode("sess-disp")
	assert.Equal(t, ccproxy.ModeNetwork, mode)
	state, err := settingsStore.GetState("http://127.0.0.1:54321/")
	require.NoError(t, err)
	assert.True(t, state.InNetworkMode)
}

// writeSettings is a tiny helper for tests that need a pre-existing settings.json.
func writeSettings(t *testing.T, path, content string) error {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

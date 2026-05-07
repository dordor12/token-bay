//go:build integration

package integration_test

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/sha256"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
	"github.com/token-bay/token-bay/tracker/internal/admission"
	"github.com/token-bay/token-bay/tracker/internal/broker"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
	"github.com/token-bay/token-bay/tracker/internal/ledger/entry"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
	"github.com/token-bay/token-bay/tracker/internal/registry"
)

// ---------------------------------------------------------------------------
// stubPusher is a synchronous PushService for integration tests.
// ---------------------------------------------------------------------------

type stubPusher struct {
	mu             sync.Mutex
	offerAccept    bool
	offerEphPub    []byte
	settlementSent bool
}

func (p *stubPusher) PushOfferTo(_ ids.IdentityID, _ *tbproto.OfferPush) (<-chan *tbproto.OfferDecision, bool) {
	ch := make(chan *tbproto.OfferDecision, 1)
	ch <- &tbproto.OfferDecision{Accept: p.offerAccept, EphemeralPubkey: p.offerEphPub}
	return ch, true
}

func (p *stubPusher) PushSettlementTo(_ ids.IdentityID, _ *tbproto.SettlementPush) (<-chan *tbproto.SettleAck, bool) {
	p.mu.Lock()
	p.settlementSent = true
	p.mu.Unlock()
	ch := make(chan *tbproto.SettleAck, 1)
	ch <- &tbproto.SettleAck{}
	return ch, true
}

// ---------------------------------------------------------------------------
// brokerE2EFixture wires up real ledger + admission + registry + broker.
// ---------------------------------------------------------------------------

type brokerE2EFixture struct {
	led        *ledger.Ledger
	store      *storage.Store
	reg        *registry.Registry
	adm        *admission.Subsystem
	subs       *broker.Subsystems
	trackerKey ed25519.PrivateKey
}

func newBrokerE2EFixture(t *testing.T, scfg config.SettlementConfig) *brokerE2EFixture {
	t.Helper()
	tmp := t.TempDir()

	// Generate a tracker keypair.
	_, trackerKey, err := ed25519.GenerateKey(crand.Reader)
	require.NoError(t, err)

	// Open a real SQLite-backed ledger.
	store, err := storage.Open(context.Background(), filepath.Join(tmp, "ledger.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	led, err := ledger.Open(store, trackerKey)
	require.NoError(t, err)

	// Build a real registry.
	reg, err := registry.New(8)
	require.NoError(t, err)

	// Build a real admission subsystem.
	admCfg := defaultAdmissionConfig()
	adm, err := admission.Open(
		admCfg,
		reg,
		trackerKey,
		admission.WithSnapshotPrefix(filepath.Join(tmp, "snapshot")),
		admission.WithTLogPath(filepath.Join(tmp, "tlog.bin")),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = adm.Close() })

	return &brokerE2EFixture{
		led:        led,
		store:      store,
		reg:        reg,
		adm:        adm,
		trackerKey: trackerKey,
	}
}

func (f *brokerE2EFixture) openBroker(t *testing.T, bcfg config.BrokerConfig, scfg config.SettlementConfig, pusher broker.PushService) *broker.Subsystems {
	return f.openBrokerWithClock(t, bcfg, scfg, pusher, time.Now)
}

func (f *brokerE2EFixture) openBrokerWithClock(t *testing.T, bcfg config.BrokerConfig, scfg config.SettlementConfig, pusher broker.PushService, now func() time.Time) *broker.Subsystems {
	t.Helper()
	deps := broker.Deps{
		Logger:    zerolog.Nop(),
		Now:       now,
		Registry:  f.reg,
		Ledger:    f.led,
		Admission: f.adm,
		Pusher:    pusher,
		Pricing:   broker.DefaultPriceTable(),
	}
	subs, err := broker.Open(bcfg, scfg, deps)
	require.NoError(t, err)
	t.Cleanup(func() { _ = subs.Close() })
	f.subs = subs
	return subs
}

// defaultAdmissionConfig mirrors the values from admission helpers_test.go.
func defaultAdmissionConfig() config.AdmissionConfig {
	return config.AdmissionConfig{
		TrialTierScore:      0.4,
		AgingAlphaPerMinute: 0.05,
		QueueTimeoutS:       300,
		ScoreWeights: config.AdmissionScoreWeights{
			SettlementReliability: 0.30,
			InverseDisputeRate:    0.10,
			Tenure:                0.20,
			NetCreditFlow:         0.30,
			BalanceCushion:        0.10,
		},
		NetFlowNormalizationConstant:          10000,
		TenureCapDays:                         30,
		StarterGrantCredits:                   1000,
		RollingWindowDays:                     30,
		PressureAdmitThreshold:                0.85,
		PressureRejectThreshold:               1.5,
		QueueCap:                              512,
		MaxAttestationScoreImported:           0.95,
		HeartbeatFreshnessDecayMaxS:           300,
		AttestationTTLSeconds:                 86400,
		AttestationIssuancePerConsumerPerHour: 6,
	}
}

// defaultBrokerConfig returns a config suitable for e2e tests.
func defaultBrokerE2EConfig() config.BrokerConfig {
	return config.BrokerConfig{
		HeadroomThreshold: 0.2,
		LoadThreshold:     5,
		ScoreWeights: config.BrokerScoreWeights{
			Reputation: 0.4, Headroom: 0.3, RTT: 0.2, Load: 0.1,
		},
		OfferTimeoutMs:          500,
		MaxOfferAttempts:        4,
		BrokerRequestRatePerSec: 100.0,
		QueueDrainIntervalMs:    100,
		InflightTerminalTTLS:    600,
	}
}

func defaultSettlementE2EConfig() config.SettlementConfig {
	return config.SettlementConfig{
		TunnelSetupMs:      5000,
		StreamIdleS:        60,
		SettlementTimeoutS: 900,
		ReservationTTLS:    1200,
		StaleTipRetries:    3,
	}
}

// seederIdentityFromPub builds a SHA-256 SPKI-style IdentityID from a raw Ed25519 pubkey.
func seederIdentityFromPub(pub ed25519.PublicKey) ids.IdentityID {
	h := sha256.Sum256(pub)
	var id ids.IdentityID
	copy(id[:], h[:])
	return id
}

// buildEnvelopeWithBalance constructs an EnvelopeSigned with a real BalanceProof
// from the ledger.
func buildEnvelopeWithBalance(t *testing.T, consumerID ids.IdentityID, led *ledger.Ledger, model string, maxIn, maxOut uint64) *tbproto.EnvelopeSigned {
	t.Helper()
	snap, err := led.SignedBalance(context.Background(), consumerID[:])
	require.NoError(t, err)

	return &tbproto.EnvelopeSigned{
		Body: &tbproto.EnvelopeBody{
			ProtocolVersion: 1,
			ConsumerId:      consumerID[:],
			Model:           model,
			MaxInputTokens:  maxIn,
			MaxOutputTokens: maxOut,
			Tier:            tbproto.PrivacyTier_PRIVACY_TIER_STANDARD,
			BodyHash:        make([]byte, 32),
			CapturedAt:      uint64(time.Now().Unix()), //nolint:gosec // G115
			Nonce:           make([]byte, 16),
			BalanceProof:    snap,
		},
	}
}

// buildSeederSignedReportE2E builds a UsageReport signed by seederPriv over
// the entry body that the broker's settlement will reconstruct. The timestamp
// is taken from the caller's fixed clock (must equal the broker's Now()) so
// the body bytes are identical when HandleUsageReport and the ledger rebuild it.
//
// In v1 the settlement always stores ConsumerSigMissing=true; the body is
// built with this flag pre-set so the seeder signature matches what the ledger
// will re-verify on append.
func buildSeederSignedReportE2E(
	t *testing.T,
	seederPriv ed25519.PrivateKey,
	requestID [16]byte,
	model string,
	inTok, outTok uint32,
	consumerID, seederID ids.IdentityID,
	led *ledger.Ledger,
	now time.Time,
) *tbproto.UsageReport {
	t.Helper()
	pt := broker.DefaultPriceTable()
	cost, err := pt.ActualCost(model, inTok, outTok)
	require.NoError(t, err)

	tipSeq, tipHash, _, err := led.Tip(context.Background())
	require.NoError(t, err)

	body, err := entry.BuildUsageEntry(entry.UsageInput{
		PrevHash:     tipHash,
		Seq:          tipSeq + 1,
		ConsumerID:   consumerID[:],
		SeederID:     seederID[:],
		Model:        model,
		InputTokens:  inTok,
		OutputTokens: outTok,
		CostCredits:  cost,
		Timestamp:    uint64(now.Unix()), //nolint:gosec // G115 — fixed past timestamp
		RequestID:    requestID[:],
		// v1: settlement always stores ConsumerSigMissing=true. The seeder
		// signs the body with this flag pre-set so the signature matches
		// what the ledger will re-verify on append.
		ConsumerSigMissing: true,
	})
	require.NoError(t, err)

	bodyBytes, err := signing.DeterministicMarshal(body)
	require.NoError(t, err)
	sig := ed25519.Sign(seederPriv, bodyBytes)

	return &tbproto.UsageReport{
		RequestId:    requestID[:],
		InputTokens:  inTok,
		OutputTokens: outTok,
		Model:        model,
		SeederSig:    sig,
	}
}

// ---------------------------------------------------------------------------
// Scenario 1: Admit → offer accept → usage_report → ledger entry
//             (ConsumerSigMissing path, fast SettlementTimeoutS=1)
// ---------------------------------------------------------------------------

func TestBrokerE2E_AdmitUsageReportLedgerEntry(t *testing.T) {
	scfg := defaultSettlementE2EConfig()
	scfg.SettlementTimeoutS = 1 // fast timer so the test doesn't take long

	fix := newBrokerE2EFixture(t, scfg)

	// Generate a seeder Ed25519 keypair. We'll use this pubkey for both the
	// "identity" and as the "ephemeral" pubkey returned in OfferDecision so
	// the signature verification in HandleUsageReport uses the same key.
	seederPub, seederPriv, err := ed25519.GenerateKey(crand.Reader)
	require.NoError(t, err)
	seederID := seederIdentityFromPub(seederPub)

	// Register seeder in the registry with our model.
	const model = "claude-sonnet-4-6"
	fix.reg.Register(registry.SeederRecord{
		IdentityID:       seederID,
		Available:        true,
		HeadroomEstimate: 0.8,
		Capabilities: registry.Capabilities{
			Models: []string{model},
			Tiers:  []tbproto.PrivacyTier{tbproto.PrivacyTier_PRIVACY_TIER_STANDARD},
		},
		LastHeartbeat: time.Now(),
	})

	// Use a fixed clock so the timestamp in the seeder's pre-signed body
	// matches what HandleUsageReport reconstructs.
	fixedNow := time.Unix(1_750_000_000, 0)
	nowFn := func() time.Time { return fixedNow }

	// Build stub pusher that accepts the offer and uses seederPub as ephemeral.
	pusher := &stubPusher{
		offerAccept: true,
		offerEphPub: seederPub,
	}

	subs := fix.openBrokerWithClock(t, defaultBrokerE2EConfig(), scfg, pusher, nowFn)

	// Consumer identity.
	var consumerID ids.IdentityID
	consumerID[0] = 0xC0

	// Issue a starter grant so the consumer has a balance.
	_, err = fix.led.IssueStarterGrant(context.Background(), consumerID[:], 1_000_000)
	require.NoError(t, err)

	// Construct an envelope with a real BalanceProof.
	env := buildEnvelopeWithBalance(t, consumerID, fix.led, model, 100, 200)

	// Submit the envelope.
	res, err := subs.Broker.Submit(context.Background(), env)
	require.NoError(t, err)
	require.Equal(t, broker.OutcomeAdmit, res.Outcome, "expected Admit outcome")
	require.NotNil(t, res.Admit)
	require.Equal(t, seederID, res.Admit.AssignedSeeder)

	requestID := res.Admit.RequestID

	// Build the usage report signed with seederPriv. The seeder pre-signs
	// with ConsumerSigMissing=true because in v1 the consumer sig is never
	// collected; the settlement builds the body with this flag so the seeder
	// must sign the same bytes.
	report := buildSeederSignedReportE2E(t, seederPriv, requestID, model, 100, 200, consumerID, seederID, fix.led, fixedNow)

	// Submit the usage report via HandleUsageReport.
	ack, err := subs.Settlement.HandleUsageReport(context.Background(), seederID, report)
	require.NoError(t, err)
	require.NotNil(t, ack)

	// Wait for the settlement timer to fire and the ledger append to complete.
	// SettlementTimeoutS=1; give it up to 5 seconds.
	deadline := time.Now().Add(5 * time.Second)
	var usageEntries []*tbproto.Entry
	for time.Now().Before(deadline) {
		entries, qErr := fix.led.EntriesSince(context.Background(), 0, 0)
		require.NoError(t, qErr)
		for _, e := range entries {
			if e.Body != nil && e.Body.Kind == tbproto.EntryKind_ENTRY_KIND_USAGE {
				usageEntries = append(usageEntries, e)
			}
		}
		if len(usageEntries) > 0 {
			break
		}
		usageEntries = nil
		time.Sleep(50 * time.Millisecond)
	}

	require.Len(t, usageEntries, 1, "expected exactly one USAGE entry in ledger")
	ue := usageEntries[0]
	require.NotNil(t, ue.Body)

	// Verify cost_credits matches the DefaultPriceTable for 100 in + 200 out of claude-sonnet-4-6.
	// In=3, Out=15 → 100*3 + 200*15 = 300 + 3000 = 3300
	require.EqualValues(t, 3300, ue.Body.CostCredits, "expected cost_credits=3300")

	// Verify ConsumerSigMissing flag (bit 0).
	const flagConsumerSigMissing uint32 = 1 << 0
	require.NotZero(t, ue.Body.Flags&flagConsumerSigMissing, "expected consumer_sig_missing=true")

	// Verify the seeder's load was released back to 0.
	rec, ok := fix.reg.Get(seederID)
	require.True(t, ok)
	require.Zero(t, rec.Load, "expected seeder Load=0 after settlement")
}

// ---------------------------------------------------------------------------
// Scenario 2: NoCapacity — empty registry
// ---------------------------------------------------------------------------

func TestBrokerE2E_NoCapacity_EmptyRegistry(t *testing.T) {
	scfg := defaultSettlementE2EConfig()
	fix := newBrokerE2EFixture(t, scfg)

	// Empty registry — no seeders registered.
	pusher := &stubPusher{offerAccept: false}
	subs := fix.openBroker(t, defaultBrokerE2EConfig(), scfg, pusher)

	var consumerID ids.IdentityID
	consumerID[0] = 0xC1

	_, err := fix.led.IssueStarterGrant(context.Background(), consumerID[:], 1_000_000)
	require.NoError(t, err)

	env := buildEnvelopeWithBalance(t, consumerID, fix.led, "claude-sonnet-4-6", 100, 200)

	res, err := subs.Broker.Submit(context.Background(), env)
	require.NoError(t, err)
	require.Equal(t, broker.OutcomeNoCapacity, res.Outcome)
	require.NotNil(t, res.NoCap)
	require.Equal(t, "no_eligible_seeder", res.NoCap.Reason)

	// Ledger should have only the starter_grant entry (no usage entry).
	entries, err := fix.led.EntriesSince(context.Background(), 0, 0)
	require.NoError(t, err)
	for _, e := range entries {
		if e.Body != nil {
			require.NotEqual(t, tbproto.EntryKind_ENTRY_KIND_USAGE, e.Body.Kind,
				"no USAGE entry should be present after NoCapacity")
		}
	}
}

// ---------------------------------------------------------------------------
// Scenario 3: InsufficientCredits → rejected; then succeed with smaller envelope
// ---------------------------------------------------------------------------

func TestBrokerE2E_InsufficientCredits_ThenSuccess(t *testing.T) {
	scfg := defaultSettlementE2EConfig()
	fix := newBrokerE2EFixture(t, scfg)

	seederPub, _, err := ed25519.GenerateKey(crand.Reader)
	require.NoError(t, err)
	seederID := seederIdentityFromPub(seederPub)

	const model = "claude-sonnet-4-6"
	fix.reg.Register(registry.SeederRecord{
		IdentityID:       seederID,
		Available:        true,
		HeadroomEstimate: 0.8,
		Capabilities: registry.Capabilities{
			Models: []string{model},
			Tiers:  []tbproto.PrivacyTier{tbproto.PrivacyTier_PRIVACY_TIER_STANDARD},
		},
		LastHeartbeat: time.Now(),
	})

	// Build pusher that will accept the second submit.
	pusher := &stubPusher{
		offerAccept: true,
		offerEphPub: seederPub,
	}
	subs := fix.openBroker(t, defaultBrokerE2EConfig(), scfg, pusher)

	var consumerID ids.IdentityID
	consumerID[0] = 0xC2

	// Grant only 100 credits. claude-sonnet-4-6 @ 1000 in + 5000 out
	// = 1000*3 + 5000*15 = 3000 + 75000 = 78000 credits > 100.
	_, err = fix.led.IssueStarterGrant(context.Background(), consumerID[:], 100)
	require.NoError(t, err)

	// First attempt: large envelope that exceeds the 100-credit balance.
	envBig := buildEnvelopeWithBalance(t, consumerID, fix.led, model, 1000, 5000)
	res, err := subs.Broker.Submit(context.Background(), envBig)
	require.NoError(t, err)
	require.Equal(t, broker.OutcomeNoCapacity, res.Outcome)
	require.NotNil(t, res.NoCap)
	require.Equal(t, "insufficient_credits", res.NoCap.Reason)

	// Re-issue more credits so we can succeed.
	_, err = fix.led.IssueStarterGrant(context.Background(), consumerID[:], 1_000_000)
	require.NoError(t, err)

	// Second attempt: small envelope within budget (100 in + 200 out = 3300 < 1_000_100).
	envSmall := buildEnvelopeWithBalance(t, consumerID, fix.led, model, 100, 200)
	res2, err := subs.Broker.Submit(context.Background(), envSmall)
	require.NoError(t, err)
	require.Equal(t, broker.OutcomeAdmit, res2.Outcome, "expected Admit for small envelope")
	require.NotNil(t, res2.Admit)
	require.Equal(t, seederID, res2.Admit.AssignedSeeder)
}

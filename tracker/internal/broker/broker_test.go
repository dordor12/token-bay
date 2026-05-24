package broker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	sharedadmission "github.com/token-bay/token-bay/shared/admission"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/admission"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
	"github.com/token-bay/token-bay/tracker/internal/registry"
)

// ---------------------------------------------------------------------------
// fakeRegistry — minimal RegistryService stub for broker tests.
// ---------------------------------------------------------------------------

type fakeRegistry struct {
	seeders []registry.SeederRecord
}

func newFakeRegistry() *fakeRegistry { return &fakeRegistry{} }

func (f *fakeRegistry) Add(rec registry.SeederRecord) {
	f.seeders = append(f.seeders, rec)
}

func (f *fakeRegistry) Match(filter registry.Filter) []registry.SeederRecord {
	out := make([]registry.SeederRecord, 0, len(f.seeders))
	for _, rec := range f.seeders {
		if filter.RequireAvailable && !rec.Available {
			continue
		}
		if filter.Model != "" {
			ok := false
			for _, m := range rec.Capabilities.Models {
				if m == filter.Model {
					ok = true
					break
				}
			}
			if !ok {
				continue
			}
		}
		if rec.HeadroomEstimate < filter.MinHeadroom {
			continue
		}
		if filter.MaxLoad > 0 && rec.Load >= filter.MaxLoad {
			continue
		}
		out = append(out, rec)
	}
	return out
}

func (f *fakeRegistry) Get(id ids.IdentityID) (registry.SeederRecord, bool) {
	for _, rec := range f.seeders {
		if rec.IdentityID == id {
			return rec, true
		}
	}
	return registry.SeederRecord{}, false
}

func (f *fakeRegistry) IncLoad(id ids.IdentityID) (int, error) {
	for i := range f.seeders {
		if f.seeders[i].IdentityID == id {
			f.seeders[i].Load++
			return f.seeders[i].Load, nil
		}
	}
	return 0, registry.ErrUnknownSeeder
}

func (f *fakeRegistry) DecLoad(id ids.IdentityID) (int, error) {
	for i := range f.seeders {
		if f.seeders[i].IdentityID == id {
			if f.seeders[i].Load > 0 {
				f.seeders[i].Load--
			}
			return f.seeders[i].Load, nil
		}
	}
	return 0, registry.ErrUnknownSeeder
}

// ---------------------------------------------------------------------------
// fakeLedger — minimal LedgerService stub.
// ---------------------------------------------------------------------------

type fakeLedger struct{}

func (fakeLedger) Tip(_ context.Context) (uint64, []byte, bool, error) {
	return 0, make([]byte, 32), false, nil
}

func (fakeLedger) AppendUsage(_ context.Context, _ ledger.UsageRecord) (*tbproto.Entry, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// fakeAdmission — minimal AdmissionService stub.
// ---------------------------------------------------------------------------

type fakeAdmission struct {
	pressure float64
}

func (a *fakeAdmission) PopReadyForBroker(_ time.Time, _ float64) (admission.QueueEntry, bool) {
	return admission.QueueEntry{}, false
}

func (a *fakeAdmission) PressureGauge() float64 { return a.pressure }

func (a *fakeAdmission) Decide(_ ids.IdentityID, _ *sharedadmission.SignedCreditAttestation, _ time.Time) admission.Result {
	return admission.Result{Outcome: admission.OutcomeAdmit}
}

// ---------------------------------------------------------------------------
// testDeps assembles a minimal Deps suitable for broker tests.
// ---------------------------------------------------------------------------

func testDeps(t *testing.T) Deps {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return Deps{
		Logger:     zerolog.Nop(),
		Registry:   newFakeRegistry(),
		Ledger:     fakeLedger{},
		Admission:  &fakeAdmission{},
		Pusher:     &fakePusher{},
		Pricing:    DefaultPriceTable(),
		TrackerKey: priv,
	}
}

func testSettlementCfg() config.SettlementConfig {
	return config.SettlementConfig{
		TunnelSetupMs:      10000,
		StreamIdleS:        60,
		SettlementTimeoutS: 900,
		ReservationTTLS:    1200,
		StaleTipRetries:    3,
	}
}

// ---------------------------------------------------------------------------
// OpenClose tests
// ---------------------------------------------------------------------------

func TestBroker_OpenClose_Idempotent(t *testing.T) {
	b, err := OpenBroker(defaultBrokerCfg(), testSettlementCfg(), testDeps(t), nil)
	require.NoError(t, err)
	require.NoError(t, b.Close())
	require.NoError(t, b.Close()) // idempotent
}

func TestBroker_OpenRequiresDeps(t *testing.T) {
	deps := testDeps(t)
	deps.Registry = nil
	_, err := OpenBroker(defaultBrokerCfg(), testSettlementCfg(), deps, nil)
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Submit test helpers
// ---------------------------------------------------------------------------

// makeAdmittedEnvelope builds an EnvelopeSigned body whose balance_proof
// claims `credits`. Sig is empty (broker.Submit doesn't verify; api/ does).
func makeAdmittedEnvelope(t *testing.T, model string, maxIn, maxOut uint64, credits int64) *tbproto.EnvelopeSigned {
	t.Helper()
	return &tbproto.EnvelopeSigned{
		Body: &tbproto.EnvelopeBody{
			ProtocolVersion: 1,
			ConsumerId:      bytesAllB(32, 0xCC),
			Model:           model,
			MaxInputTokens:  maxIn,
			MaxOutputTokens: maxOut,
			Tier:            tbproto.PrivacyTier_PRIVACY_TIER_STANDARD,
			BodyHash:        bytesAllB(32, 0x01),
			CapturedAt:      1700000000,
			Nonce:           bytesAllB(16, 0x02),
			BalanceProof: &tbproto.SignedBalanceSnapshot{
				Body: &tbproto.BalanceSnapshotBody{Credits: credits},
			},
		},
	}
}

// withFakePusher swaps the Deps' Pusher for one that delivers a fixed offer
// decision. Returns the Pusher so the test can drive subsequent decisions.
func withFakePusher(t *testing.T, deps *Deps) *fakePusher {
	t.Helper()
	p := &fakePusher{offerCh: make(chan *tbproto.OfferDecision, 8), ok: true}
	deps.Pusher = p
	return p
}

// queueDecision pushes one offer decision onto the fake pusher's channel.
func queueDecision(p *fakePusher, accept bool, ephemeralPub []byte) {
	p.offerCh <- &tbproto.OfferDecision{Accept: accept, EphemeralPubkey: ephemeralPub}
}

// ---------------------------------------------------------------------------
// Submit tests
// ---------------------------------------------------------------------------

func TestSubmit_AdmitFirstCandidate(t *testing.T) {
	deps := testDeps(t)
	fr := newFakeRegistry()
	fr.Add(seederRecord(t, ids.IdentityID{1}, 0.9, "claude-sonnet-4-6"))
	deps.Registry = fr
	p := withFakePusher(t, &deps)
	queueDecision(p, true, bytesAllB(32, 0xCC))

	b, err := OpenBroker(defaultBrokerCfg(), testSettlementCfg(), deps, nil)
	require.NoError(t, err)
	defer b.Close()

	env := makeAdmittedEnvelope(t, "claude-sonnet-4-6", 100, 200, 1_000_000)
	res, err := b.Submit(context.Background(), env)
	require.NoError(t, err)
	require.Equal(t, OutcomeAdmit, res.Outcome)

	require.NotNil(t, res.Admit)
	require.Equal(t, ids.IdentityID{1}, res.Admit.AssignedSeeder)
	require.Equal(t, 16, len(res.Admit.ReservationToken))

	var rid [16]byte
	copy(rid[:], res.Admit.ReservationToken)
	consumer, seeder, ok := b.LookupAssignment(rid)
	require.True(t, ok)
	require.Equal(t, ids.IdentityID{1}, seeder)
	require.NotEqual(t, ids.IdentityID{}, consumer)
}

func TestLookupAssignment_UnknownRequest(t *testing.T) {
	deps := testDeps(t)
	deps.Registry = newFakeRegistry()
	withFakePusher(t, &deps)

	b, err := OpenBroker(defaultBrokerCfg(), testSettlementCfg(), deps, nil)
	require.NoError(t, err)
	defer b.Close()

	_, _, ok := b.LookupAssignment([16]byte{0xFE})
	require.False(t, ok)
}

func TestSubmit_InsufficientCredits(t *testing.T) {
	deps := testDeps(t)
	fr := newFakeRegistry()
	fr.Add(seederRecord(t, ids.IdentityID{1}, 0.9, "claude-sonnet-4-6"))
	deps.Registry = fr
	withFakePusher(t, &deps)

	b, err := OpenBroker(defaultBrokerCfg(), testSettlementCfg(), deps, nil)
	require.NoError(t, err)
	defer b.Close()

	// claude-sonnet-4-6 = 3 in / 15 out per token; 100 in + 200 out = 3300 cost
	env := makeAdmittedEnvelope(t, "claude-sonnet-4-6", 100, 200, 1000) // < cost
	res, err := b.Submit(context.Background(), env)
	require.NoError(t, err)
	require.Equal(t, OutcomeNoCapacity, res.Outcome)
	require.Equal(t, "insufficient_credits", res.NoCap.Reason)
}

func TestSubmit_NoEligibleSeeder(t *testing.T) {
	deps := testDeps(t)
	deps.Registry = newFakeRegistry() // empty
	withFakePusher(t, &deps)

	b, err := OpenBroker(defaultBrokerCfg(), testSettlementCfg(), deps, nil)
	require.NoError(t, err)
	defer b.Close()

	env := makeAdmittedEnvelope(t, "claude-sonnet-4-6", 100, 200, 1_000_000)
	res, err := b.Submit(context.Background(), env)
	require.NoError(t, err)
	require.Equal(t, OutcomeNoCapacity, res.Outcome)
	require.Equal(t, "no_eligible_seeder", res.NoCap.Reason)
}

func TestSubmit_AllRejected(t *testing.T) {
	deps := testDeps(t)
	fr := newFakeRegistry()
	cfg := defaultBrokerCfg()
	for i := 0; i < cfg.MaxOfferAttempts; i++ {
		fr.Add(seederRecord(t, ids.IdentityID{byte(i + 1)}, 0.9, "claude-sonnet-4-6"))
	}
	deps.Registry = fr
	p := withFakePusher(t, &deps)
	for i := 0; i < cfg.MaxOfferAttempts; i++ {
		queueDecision(p, false, nil)
	}

	b, err := OpenBroker(cfg, testSettlementCfg(), deps, nil)
	require.NoError(t, err)
	defer b.Close()

	env := makeAdmittedEnvelope(t, "claude-sonnet-4-6", 100, 200, 1_000_000)
	res, err := b.Submit(context.Background(), env)
	require.NoError(t, err)
	require.Equal(t, OutcomeNoCapacity, res.Outcome)
	require.Equal(t, "all_seeders_rejected", res.NoCap.Reason)
}

func TestSubmit_ReservationReleasedOnNoCapacity(t *testing.T) {
	deps := testDeps(t)
	deps.Registry = newFakeRegistry()
	withFakePusher(t, &deps)

	b, err := OpenBroker(defaultBrokerCfg(), testSettlementCfg(), deps, nil)
	require.NoError(t, err)
	defer b.Close()

	env := makeAdmittedEnvelope(t, "claude-sonnet-4-6", 100, 200, 1_000_000)
	var consumer ids.IdentityID
	copy(consumer[:], env.Body.ConsumerId)
	_, err = b.Submit(context.Background(), env)
	require.NoError(t, err)
	require.Equal(t, uint64(0), b.mgr.Reservations.Reserved(consumer))
}

func TestSubmit_RecordsAcceptOutcome(t *testing.T) {
	deps := testDeps(t)
	fr := newFakeRegistry()
	seederID := ids.IdentityID{42}
	fr.Add(seederRecord(t, seederID, 0.9, "claude-sonnet-4-6"))
	deps.Registry = fr
	p := withFakePusher(t, &deps)
	queueDecision(p, true, bytesAllB(32, 0xAA))

	rep := newStubReputation()
	deps.Reputation = rep

	b, err := OpenBroker(defaultBrokerCfg(), testSettlementCfg(), deps, nil)
	require.NoError(t, err)
	defer b.Close()

	env := makeAdmittedEnvelope(t, "claude-sonnet-4-6", 100, 200, 1_000_000)
	res, err := b.Submit(context.Background(), env)
	require.NoError(t, err)
	require.Equal(t, OutcomeAdmit, res.Outcome)

	outs := rep.outcomes()
	require.Len(t, outs, 1)
	require.Equal(t, seederID, outs[0].ID)
	require.Equal(t, "accept", outs[0].Outcome)
}

func TestSubmit_UnknownModel(t *testing.T) {
	deps := testDeps(t)
	deps.Registry = newFakeRegistry()
	withFakePusher(t, &deps)

	b, err := OpenBroker(defaultBrokerCfg(), testSettlementCfg(), deps, nil)
	require.NoError(t, err)
	defer b.Close()

	env := makeAdmittedEnvelope(t, "gpt-9", 1, 1, 1_000_000)
	_, err = b.Submit(context.Background(), env)
	require.ErrorIs(t, err, ErrUnknownModel)
}

package admission

import (
	"crypto/ed25519"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	admission "github.com/token-bay/token-bay/shared/admission"
	"github.com/token-bay/token-bay/shared/ids"
	signing "github.com/token-bay/token-bay/shared/signing"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/registry"
)

// fixtureTrackerSeed is the deterministic seed for the test tracker
// keypair. Stable across runs so attestation goldens (if added) reproduce.
var fixtureTrackerSeed = []byte("admission-fixture-tracker-seed-1")

// fixturePeerSeed is the deterministic seed for a "remote" tracker
// (peer) keypair used in attestation-validation tests.
var fixturePeerSeed = []byte("admission-fixture-peer-seed-v1-x")

// defaultScoreConfig returns the AdmissionConfig used by openTempSubsystem.
// Mirrors the spec defaults from admission-design §9.3.
func defaultScoreConfig() config.AdmissionConfig {
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

// openTempSubsystem returns a wired *Subsystem with fixed clock, default
// config, empty registry, and the fixture tracker keypair. Cleanup hooks
// close it on test completion.
func openTempSubsystem(t *testing.T, opts ...Option) (*Subsystem, ids.IdentityID) {
	t.Helper()
	require.Len(t, fixtureTrackerSeed, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(fixtureTrackerSeed)

	reg, err := registry.New(16)
	require.NoError(t, err)

	s, err := Open(defaultScoreConfig(), reg, priv, opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s, s.trackerID
}

// fixturePeerKeypair returns the fixture peer-tracker (Ed25519) keypair
// + its derived IdentityID. Used in attestation-validation tests where
// the peer is the "issuer" of imported attestations.
func fixturePeerKeypair(t *testing.T) (ed25519.PrivateKey, ids.IdentityID) {
	t.Helper()
	require.Len(t, fixturePeerSeed, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(fixturePeerSeed)
	pub := priv.Public().(ed25519.PublicKey)
	var id ids.IdentityID
	copy(id[:], pub[:32])
	return priv, id
}

// staticPeers is a deterministic PeerSet implementation for tests.
type staticPeers map[ids.IdentityID]bool

func (s staticPeers) Contains(id ids.IdentityID) bool { return s[id] }

// validAttestationBody returns a populated body that passes
// admission.ValidateCreditAttestationBody. Tests mutate single fields
// (Score, ExpiresAt, etc.) to test specific code paths.
func validAttestationBody(now time.Time, peerID ids.IdentityID, consumerByte byte) *admission.CreditAttestationBody {
	id := makeID(consumerByte)
	return &admission.CreditAttestationBody{
		IdentityId:            id[:],
		IssuerTrackerId:       peerID[:],
		Score:                 7000,
		TenureDays:            60,
		SettlementReliability: 9000,
		DisputeRate:           100,
		NetCreditFlow_30D:     50000,
		BalanceCushionLog2:    1,
		ComputedAt:            uint64(now.Add(-time.Hour).Unix()),     //nolint:gosec // G115 — post-1970
		ExpiresAt:             uint64(now.Add(23 * time.Hour).Unix()), //nolint:gosec // G115 — post-1970
	}
}

// signFixtureAttestation signs body with priv via shared/signing helpers.
func signFixtureAttestation(t *testing.T, priv ed25519.PrivateKey, body *admission.CreditAttestationBody) *admission.SignedCreditAttestation {
	t.Helper()
	sig, err := signing.SignCreditAttestation(priv, body)
	require.NoError(t, err)
	return &admission.SignedCreditAttestation{Body: body, TrackerSig: sig}
}

// fixedClock returns the same Now() value until Advance shifts it.
type fixedClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFixedClock(t time.Time) *fixedClock { return &fixedClock{now: t} }

// Now returns the current cached time.
func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance shifts the cached time by d.
func (c *fixedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// staticClockFn returns a func that always reports t.
func staticClockFn(t time.Time) func() time.Time { return func() time.Time { return t } }

// registerSeeder is a one-line registry mutation used by supply-aggregator
// tests.
func registerSeeder(t *testing.T, reg *registry.Registry, id ids.IdentityID, headroom float64, lastHB time.Time) {
	t.Helper()
	reg.Register(registry.SeederRecord{
		IdentityID:       id,
		HeadroomEstimate: headroom,
		LastHeartbeat:    lastHB,
		Available:        true,
		ReputationScore:  1.0,
	})
}

// makeID builds an IdentityID with all bytes set to b.
func makeID(b byte) ids.IdentityID {
	var id ids.IdentityID
	for i := range id {
		id[i] = b
	}
	return id
}

// makeIDi builds an IdentityID encoding i in the first two bytes.
// Used by queue-overflow tests that need many distinct identities.
func makeIDi(i int) ids.IdentityID {
	var id ids.IdentityID
	id[0] = byte(i & 0xff)
	id[1] = byte((i >> 8) & 0xff)
	return id
}

// fixedRand returns a deterministic *rand.Rand for jitter tests.
func fixedRand(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) } //nolint:gosec // G404 — tests

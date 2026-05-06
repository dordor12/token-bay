package admission

import (
	"context"
	"crypto/ed25519"
	"math/rand"
	"os"
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
// close it on test completion. Accepts testing.TB so benchmarks can call
// it too.
func openTempSubsystem(tb testing.TB, opts ...Option) (*Subsystem, ids.IdentityID) {
	tb.Helper()
	require.Len(tb, fixtureTrackerSeed, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(fixtureTrackerSeed)

	reg, err := registry.New(16)
	require.NoError(tb, err)

	s, err := Open(defaultScoreConfig(), reg, priv, opts...)
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = s.Close() })
	return s, s.trackerID
}

// fixturePeerKeypair returns the fixture peer-tracker (Ed25519) keypair
// + its derived IdentityID. Used in attestation-validation tests where
// the peer is the "issuer" of imported attestations.
func fixturePeerKeypair(tb testing.TB) (ed25519.PrivateKey, ids.IdentityID) {
	tb.Helper()
	require.Len(tb, fixturePeerSeed, ed25519.SeedSize)
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

// corruptByteAt flips one bit in the byte at offset off in the file at
// path. Used by replay tests to simulate on-disk corruption.
func corruptByteAt(t *testing.T, path string, off int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	require.NoError(t, err)
	defer f.Close()
	buf := make([]byte, 1)
	_, err = f.ReadAt(buf, off)
	require.NoError(t, err)
	buf[0] ^= 0xff
	_, err = f.WriteAt(buf, off)
	require.NoError(t, err)
}

// appendGarbageBytes appends n random bytes to the file at path so a
// would-be replay sees a partial trailing frame.
func appendGarbageBytes(t *testing.T, path string, n int) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	defer f.Close()
	garbage := make([]byte, n)
	for i := range garbage {
		garbage[i] = byte(i + 1)
	}
	_, err = f.Write(garbage)
	require.NoError(t, err)
}

// fakeLedgerSource is the deterministic LedgerSource used by replay
// cross-check tests.
type fakeLedgerSource struct {
	mu     sync.Mutex
	events []LedgerEvent
}

// EventsAfter implements the LedgerSource contract; minSeq is unused
// (the fake's events list is treated as authoritative).
func (s *fakeLedgerSource) EventsAfter(_ context.Context, _ uint64) ([]LedgerEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]LedgerEvent, len(s.events))
	copy(out, s.events)
	return out, nil
}

// append is used by failure-mode tests (Task 9, Task 13) to grow the
// fake ledger after the writer crashes. Marked nolint:unused so plan-3
// tasks 1-7 build; later tasks consume it.
//
//nolint:unused // used by Task 9 + Task 13
func (s *fakeLedgerSource) append(ev LedgerEvent) {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
}

// allowAllPeerSet treats every issuer as a recognized peer.
type allowAllPeerSet struct{}

func (allowAllPeerSet) Contains(ids.IdentityID) bool { return true }

// rejectAllPeerSet rejects every issuer (the v1 default behavior).
type rejectAllPeerSet struct{}

func (rejectAllPeerSet) Contains(ids.IdentityID) bool { return false }

// signedTestAttestation builds a peer-issued attestation that passes every
// validation step except the optional clamp. Useful as a baseline to
// tamper with in §10 #17 (forged) tests.
func signedTestAttestation(tb testing.TB, s *Subsystem) *admission.SignedCreditAttestation {
	tb.Helper()
	return signedTestAttestationWithScore(tb, s, 7000)
}

// signedTestAttestationWithScore builds a peer-issued attestation with a
// declared score (raw uint32, fixed-point /10000). §10 #19 uses the
// inflated value to test the MaxAttestationScoreImported clamp.
func signedTestAttestationWithScore(tb testing.TB, s *Subsystem, rawScore uint32) *admission.SignedCreditAttestation {
	tb.Helper()
	now := s.nowFn()
	priv, peerID := fixturePeerKeypair(tb)
	consumerID := makeID(0xC1)
	body := &admission.CreditAttestationBody{
		IdentityId:            consumerID[:],
		IssuerTrackerId:       peerID[:],
		Score:                 rawScore,
		TenureDays:            60,
		SettlementReliability: 9000,
		DisputeRate:           100,
		NetCreditFlow_30D:     50000,
		BalanceCushionLog2:    1,
		ComputedAt:            uint64(now.Add(-time.Hour).Unix()),     //nolint:gosec // G115 — post-1970
		ExpiresAt:             uint64(now.Add(23 * time.Hour).Unix()), //nolint:gosec // G115 — post-1970
	}
	sig, err := signing.SignCreditAttestation(priv, body)
	require.NoError(tb, err)
	return &admission.SignedCreditAttestation{Body: body, TrackerSig: sig}
}

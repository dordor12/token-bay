package admission

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// §10 #17 — Forged attestation (body tampered post-sign) → trial-tier score.
func TestAcceptance_S10_17_ForgedAttestationFallsThrough(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithPeerSet(allowAllPeerSet{}))
	s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 0.5})

	att := signedTestAttestation(t, s)
	// Tamper post-sign — flip a body field's value so the signature no
	// longer matches the body bytes.
	att.Body.Score = 9999

	res := s.Decide(makeID(0xC1), att, now)
	assert.InDelta(t, s.cfg.TrialTierScore, res.CreditUsed, 1e-9,
		"signature verification rejected → trial-tier score")
}

// §10 #18 — Federation peer ejected after issuing attestation.
func TestAcceptance_S10_18_EjectedPeerAttestationRejected(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithPeerSet(rejectAllPeerSet{}))
	s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 0.5})

	att := signedTestAttestation(t, s)
	res := s.Decide(makeID(0xC1), att, now)
	assert.InDelta(t, s.cfg.TrialTierScore, res.CreditUsed, 1e-9,
		"ejected peer's attestation was discarded")
}

// §10 #19 — Inflated peer attestation → clamped at MaxAttestationScoreImported.
func TestAcceptance_S10_19_InflatedScoreClamped(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithPeerSet(allowAllPeerSet{}))
	s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 0.5})

	// Score 99999 (= 9.9999 in fixed-point). Clamp = 0.95 by default.
	att := signedTestAttestationWithScore(t, s, 99999)
	res := s.Decide(makeID(0xC1), att, now)
	assert.LessOrEqual(t, res.CreditUsed, s.cfg.MaxAttestationScoreImported,
		"score clamped at MaxAttestationScoreImported")
}

// §10 #20 — /admission/queue requires operator auth.
func TestAcceptance_S10_20_AdminQueueRequiresAuth(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	srv := newAdminHTTPServer(t, s)
	defer srv.Close()

	resp := adminReq(t, "GET", srv.URL+"/admission/queue", "", "")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	resp = adminReq(t, "GET", srv.URL+"/admission/queue", "test-token", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

package reputation

import (
	"math"
	"testing"
	"time"
)

func TestScore_AllDefaults(t *testing.T) {
	// No signals at all: completion=consumer_sat=seeder_fair=1.0,
	// longevity=0, audit_penalty=0 → 0.3+0.3+0.2+0+0 = 0.8.
	in := newDefaultInputs(time.Unix(0, 0), time.Unix(0, 0))
	got := computeScore(in)
	if math.Abs(got-0.8) > 0.0001 {
		t.Errorf("computeScore(defaults) = %f, want 0.8", got)
	}
}

func TestScore_LongevityCurve(t *testing.T) {
	base := time.Unix(0, 0)
	in := newDefaultInputs(base, base.Add(365*24*time.Hour))
	got := computeScore(in)
	// longevity_bonus → ~1.0; defaults give 0.3+0.3+0.2+0.2 = 1.0.
	if math.Abs(got-1.0) > 0.0001 {
		t.Errorf("computeScore(1y) = %f, want 1.0", got)
	}
}

func TestScore_AuditRecencyPenaltyDecays(t *testing.T) {
	base := time.Unix(0, 0)
	fresh := newDefaultInputs(base, base)
	fresh.LastAuditAt = base // Δt = 0 → penalty = 1.0 → -0.4
	if got := computeScore(fresh); math.Abs(got-0.4) > 0.0001 {
		t.Errorf("score with fresh audit = %f, want 0.4", got)
	}

	now7d := base.Add(7 * 24 * time.Hour)
	aged := newDefaultInputs(now7d, now7d) // firstSeen == now → longevity=0, isolating penalty
	aged.LastAuditAt = base                // Δt = 7d → penalty = e^-1
	want := 0.8 - 0.4*math.Exp(-1)
	if got := computeScore(aged); math.Abs(got-want) > 0.0001 {
		t.Errorf("score with 7d-old audit = %f, want %f", got, want)
	}
}

func TestScore_Clamped(t *testing.T) {
	// Force every term low and audit penalty high.
	in := scoreInputs{
		CompletionHealth:     0,
		ConsumerSatisfaction: 0,
		SeederFairness:       0,
		FirstSeenAt:          time.Unix(0, 0),
		Now:                  time.Unix(0, 0),
		LastAuditAt:          time.Unix(0, 0),
	}
	got := computeScore(in)
	if got < 0 || got > 1 {
		t.Errorf("computeScore unclamped: %f", got)
	}
	if got != 0 {
		t.Errorf("computeScore = %f, want 0 (clamp)", got)
	}
}

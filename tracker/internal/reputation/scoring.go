package reputation

import (
	"math"
	"time"
)

// Spec §5 weights. Constants in MVP; promotion to config-tunable is
// open question 10.3.
const (
	weightCompletionHealth     = 0.3
	weightConsumerSatisfaction = 0.3
	weightSeederFairness       = 0.2
	weightLongevityBonus       = 0.2
	weightAuditRecencyPenalty  = 0.4
)

// scoreInputs is everything computeScore needs. The evaluator
// constructs one of these per identity per cycle.
type scoreInputs struct {
	CompletionHealth     float64 // [0,1] — defaults to 1.0 for "no data"
	ConsumerSatisfaction float64 // [0,1] — defaults to 1.0
	SeederFairness       float64 // [0,1] — defaults to 1.0
	FirstSeenAt          time.Time
	Now                  time.Time
	LastAuditAt          time.Time // zero means "no audit ever"
	HasData              bool      //nolint:unused // wired in later evaluator task
}

// computeScore implements spec §5 verbatim. Callers must initialize the
// three rate terms to 1.0 before applying signal-derived adjustments;
// newDefaultInputs is the helper.
func computeScore(in scoreInputs) float64 {
	raw := weightCompletionHealth*in.CompletionHealth +
		weightConsumerSatisfaction*in.ConsumerSatisfaction +
		weightSeederFairness*in.SeederFairness +
		weightLongevityBonus*longevityBonus(in.FirstSeenAt, in.Now) -
		weightAuditRecencyPenalty*auditRecencyPenalty(in.LastAuditAt, in.Now)
	return clamp01(raw)
}

// newDefaultInputs returns a scoreInputs with the three rate terms
// initialized to 1.0 ("no data" defaults per spec §5).
func newDefaultInputs(firstSeen, now time.Time) scoreInputs {
	return scoreInputs{
		CompletionHealth:     1.0,
		ConsumerSatisfaction: 1.0,
		SeederFairness:       1.0,
		FirstSeenAt:          firstSeen,
		Now:                  now,
	}
}

func longevityBonus(firstSeen, now time.Time) float64 {
	if firstSeen.IsZero() || !now.After(firstSeen) {
		return 0
	}
	days := now.Sub(firstSeen).Hours() / 24.0
	return math.Min(1.0, math.Log(days+1)/math.Log(365))
}

func auditRecencyPenalty(lastAudit, now time.Time) float64 {
	if lastAudit.IsZero() {
		return 0
	}
	dt := now.Sub(lastAudit).Hours() / 24.0 // days
	if dt < 0 {
		dt = 0
	}
	return math.Exp(-dt / 7.0) // half-life-ish: 7d
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

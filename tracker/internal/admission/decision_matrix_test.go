package admission

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sharedadmission "github.com/token-bay/token-bay/shared/admission"
)

type attestationKind uint8

const (
	attNone attestationKind = iota
	attValid
	attExpired
	attForged
	attEjectedIssuer
)

type historyKind uint8

const (
	histNone historyKind = iota
	histSome
	histExtensive
)

type pressureKind uint8

const (
	pressLow  pressureKind = iota // < 0.85 → ADMIT regardless
	pressMid                      // 0.85 ≤ p < 1.5 → QUEUE if room
	pressHigh                     // ≥ 1.5 → REJECT
)

type matrixRow struct {
	att   attestationKind
	hist  historyKind
	press pressureKind
	want  Outcome
}

func TestDecisionMatrix_FullProduct(t *testing.T) {
	rows := []matrixRow{}
	for _, a := range []attestationKind{attNone, attValid, attExpired, attForged, attEjectedIssuer} {
		for _, h := range []historyKind{histNone, histSome, histExtensive} {
			for _, p := range []pressureKind{pressLow, pressMid, pressHigh} {
				rows = append(rows, matrixRow{
					att: a, hist: h, press: p,
					want: expectedOutcome(p),
				})
			}
		}
	}

	for _, row := range rows {
		row := row
		t.Run(matrixName(row), func(t *testing.T) {
			now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
			peerPriv, peerID := fixturePeerKeypair(t)
			peers := staticPeers{}
			if row.att == attValid || row.att == attExpired || row.att == attForged {
				peers[peerID] = true // recognized issuer
			}
			// attEjectedIssuer: peer is NOT in the set even though att is well-formed
			s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithPeerSet(peers))

			// Set up local history per row.
			id := makeID(0xC0)
			switch row.hist {
			case histSome:
				st := consumerShardFor(s.consumerShards, id).getOrInit(id, now.AddDate(0, 0, -10))
				st.SettlementBuckets[0] = DayBucket{Total: 10, A: 9, DayStamp: stripToDay(now)}
				st.LastBalanceSeen = 1000
			case histExtensive:
				st := consumerShardFor(s.consumerShards, id).getOrInit(id, now.AddDate(0, 0, -30))
				st.SettlementBuckets[0] = DayBucket{Total: 100, A: 100, DayStamp: stripToDay(now)}
				st.LastBalanceSeen = 50000
			}

			// Build attestation per row.
			var att *sharedadmission.SignedCreditAttestation
			switch row.att {
			case attValid, attEjectedIssuer:
				body := validAttestationBody(now, peerID, 0xC0)
				att = signFixtureAttestation(t, peerPriv, body)
			case attExpired:
				body := validAttestationBody(now.Add(-48*time.Hour), peerID, 0xC0)
				body.ExpiresAt = uint64(now.Add(-1 * time.Hour).Unix())
				att = signFixtureAttestation(t, peerPriv, body)
			case attForged:
				body := validAttestationBody(now, peerID, 0xC0)
				att = signFixtureAttestation(t, peerPriv, body)
				att.Body.Score = 9999 // post-sign mutation
			}

			// Set up pressure per row.
			switch row.press {
			case pressLow:
				s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 0.5})
			case pressMid:
				s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 1.0})
			case pressHigh:
				s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 2.0})
			}

			res := s.Decide(id, att, now)
			require.Equal(t, row.want, res.Outcome,
				"att=%v hist=%v press=%v → got %v, want %v",
				row.att, row.hist, row.press, res.Outcome, row.want)

			// Sanity: REJECT must carry retry_after in [60, 600].
			if res.Outcome == OutcomeReject {
				assert.GreaterOrEqual(t, res.Rejected.RetryAfterS, uint32(60))
				assert.LessOrEqual(t, res.Rejected.RetryAfterS, uint32(600))
			}
		})
	}
}

// expectedOutcome derives the canonical outcome for a (pressure) bucket.
// Note: history and attestation kind affect CreditUsed but not Outcome
// for v1's threshold-based decision — admit/queue/reject is purely
// pressure-driven (§5.1 step 4). Higher credit changes queue *priority*
// (Task 8) but doesn't change the admit/queue/reject category.
func expectedOutcome(p pressureKind) Outcome {
	switch p {
	case pressLow:
		return OutcomeAdmit
	case pressMid:
		return OutcomeQueue
	default:
		return OutcomeReject
	}
}

func matrixName(r matrixRow) string {
	atts := []string{"none", "valid", "expired", "forged", "ejected"}
	hists := []string{"hist=none", "hist=some", "hist=extensive"}
	press := []string{"press=low", "press=mid", "press=high"}
	return atts[r.att] + "/" + hists[r.hist] + "/" + press[r.press]
}

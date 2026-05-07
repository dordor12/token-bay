package broker

import (
	"bytes"
	"sort"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/registry"
)

// Candidate is a registry seeder paired with its computed selection score.
type Candidate struct {
	Record registry.SeederRecord
	Score  float64
}

// Score implements broker-design §4.4.2:
//
//	score = α·rep + β·headroom − γ·rttMs − δ·load
//
// In v1, callers pass rttMs=0 and the γ term collapses (broker-design §4.4.3).
func Score(rec registry.SeederRecord, w config.BrokerScoreWeights, repScore float64, rttMs float64) float64 {
	return w.Reputation*repScore +
		w.Headroom*rec.HeadroomEstimate -
		w.RTT*rttMs -
		w.Load*float64(rec.Load)
}

// Pick filters a registry snapshot down to eligible candidates and ranks
// them by Score descending. Tie-break is lex-order on IdentityID for
// deterministic test output.
//
// The filter is broker-design §4.4.1:
//   - record.Available
//   - not in alreadyTried
//   - capabilities.Models contains env.Model
//   - capabilities.Tiers contains env.Tier
//   - HeadroomEstimate ≥ cfg.HeadroomThreshold
//   - cfg.LoadThreshold == 0 OR Load < cfg.LoadThreshold
//   - !reputation.IsFrozen(id)
func Pick(snapshot []registry.SeederRecord, env *tbproto.EnvelopeBody,
	w config.BrokerScoreWeights, rep ReputationService, cfg config.BrokerConfig,
	alreadyTried []ids.IdentityID,
) []Candidate {
	triedSet := make(map[ids.IdentityID]struct{}, len(alreadyTried))
	for _, id := range alreadyTried {
		triedSet[id] = struct{}{}
	}
	out := make([]Candidate, 0, len(snapshot))
	for _, rec := range snapshot {
		if !rec.Available {
			continue
		}
		if _, tried := triedSet[rec.IdentityID]; tried {
			continue
		}
		if !containsString(rec.Capabilities.Models, env.Model) {
			continue
		}
		if !containsTier(rec.Capabilities.Tiers, env.Tier) {
			continue
		}
		if rec.HeadroomEstimate < cfg.HeadroomThreshold {
			continue
		}
		if cfg.LoadThreshold > 0 && rec.Load >= cfg.LoadThreshold {
			continue
		}
		if rep.IsFrozen(rec.IdentityID) {
			continue
		}
		repScore, _ := rep.Score(rec.IdentityID)
		out = append(out, Candidate{
			Record: rec,
			Score:  Score(rec, w, repScore, 0 /* v1 RTT placeholder */),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		a := out[i].Record.IdentityID.Bytes()
		b := out[j].Record.IdentityID.Bytes()
		return bytes.Compare(a[:], b[:]) < 0
	})
	return out
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func containsTier(xs []tbproto.PrivacyTier, want tbproto.PrivacyTier) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

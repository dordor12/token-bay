package broker

import (
	"sync"
	"testing"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/admission"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/registry"
)

func defaultBrokerCfg() config.BrokerConfig {
	return config.BrokerConfig{
		HeadroomThreshold: 0.2,
		LoadThreshold:     5,
		ScoreWeights: config.BrokerScoreWeights{
			Reputation: 0.4, Headroom: 0.3, RTT: 0.2, Load: 0.1,
		},
		OfferTimeoutMs:          1500,
		MaxOfferAttempts:        4,
		BrokerRequestRatePerSec: 2.0,
		QueueDrainIntervalMs:    1000,
		InflightTerminalTTLS:    600,
	}
}

func defaultWeights() config.BrokerScoreWeights {
	return defaultBrokerCfg().ScoreWeights
}

// seederRecord builds a SeederRecord for tests with sane defaults.
func seederRecord(t *testing.T, id ids.IdentityID, headroom float64, model string) registry.SeederRecord {
	t.Helper()
	return registry.SeederRecord{
		IdentityID:       id,
		Available:        true,
		HeadroomEstimate: headroom,
		Capabilities: registry.Capabilities{
			Models: []string{model},
			Tiers:  []tbproto.PrivacyTier{tbproto.PrivacyTier_PRIVACY_TIER_STANDARD},
		},
	}
}

// recordedOutcome captures a single RecordOfferOutcome call for test assertions.
type recordedOutcome struct {
	ID      ids.IdentityID
	Outcome string
}

// stubReputation lets tests control Score / IsFrozen per id and inspect
// outcomes recorded via RecordOfferOutcome and ledger events via OnLedgerEvent.
// All recording fields are guarded by mu since settlement dispatches from a
// background goroutine.
type stubReputation struct {
	scores               map[ids.IdentityID]float64
	frozen               map[ids.IdentityID]bool
	mu                   sync.Mutex
	recordedOutcomes     []recordedOutcome
	recordedLedgerEvents []admission.LedgerEvent
}

func newStubReputation() *stubReputation {
	return &stubReputation{
		scores: map[ids.IdentityID]float64{},
		frozen: map[ids.IdentityID]bool{},
	}
}

func (s *stubReputation) Score(id ids.IdentityID) (float64, bool) {
	v, ok := s.scores[id]
	return v, ok
}
func (s *stubReputation) IsFrozen(id ids.IdentityID) bool { return s.frozen[id] }
func (s *stubReputation) RecordOfferOutcome(id ids.IdentityID, outcome string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordedOutcomes = append(s.recordedOutcomes, recordedOutcome{ID: id, Outcome: outcome})
	return nil
}

func (s *stubReputation) OnLedgerEvent(ev admission.LedgerEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordedLedgerEvents = append(s.recordedLedgerEvents, ev)
}

// outcomes returns a copy of recordedOutcomes for safe inspection.
func (s *stubReputation) outcomes() []recordedOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]recordedOutcome, len(s.recordedOutcomes))
	copy(out, s.recordedOutcomes)
	return out
}

// ledgerEvents returns a copy of recordedLedgerEvents for safe inspection.
func (s *stubReputation) ledgerEvents() []admission.LedgerEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]admission.LedgerEvent, len(s.recordedLedgerEvents))
	copy(out, s.recordedLedgerEvents)
	return out
}

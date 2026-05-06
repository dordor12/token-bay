package broker

import (
	"context"
	"crypto/ed25519"
	"time"

	"github.com/rs/zerolog"

	sharedadmission "github.com/token-bay/token-bay/shared/admission"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/admission"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
	"github.com/token-bay/token-bay/tracker/internal/registry"
)

// RegistryService is the slice of *registry.Registry the broker depends on.
type RegistryService interface {
	Match(registry.Filter) []registry.SeederRecord
	Get(ids.IdentityID) (registry.SeederRecord, bool)
	IncLoad(ids.IdentityID) (int, error)
	DecLoad(ids.IdentityID) (int, error)
}

// LedgerService is the slice of *ledger.Ledger the broker + settlement use.
type LedgerService interface {
	Tip(ctx context.Context) (uint64, []byte, bool, error)
	AppendUsage(ctx context.Context, r ledger.UsageRecord) (*tbproto.Entry, error)
}

// AdmissionService is the slice of *admission.Subsystem the broker uses.
// Decide is invoked by api/, not broker, but kept on the union so api can
// supply *admission.Subsystem once and have all broker collaborators wired.
type AdmissionService interface {
	PopReadyForBroker(now time.Time, minServePriority float64) (admission.QueueEntry, bool)
	PressureGauge() float64
	Decide(consumerID ids.IdentityID, att *sharedadmission.SignedCreditAttestation, now time.Time) admission.Result
}

// ReputationService is advisory: nil falls back to fallbackReputation.
type ReputationService interface {
	Score(ids.IdentityID) (float64, bool)
	IsFrozen(ids.IdentityID) bool
	RecordOfferOutcome(seeder ids.IdentityID, outcome string)
}

// PushService is the slice of *server.Server the broker uses for offer +
// settlement push streams.
type PushService interface {
	PushOfferTo(ids.IdentityID, *tbproto.OfferPush) (<-chan *tbproto.OfferDecision, bool)
	PushSettlementTo(ids.IdentityID, *tbproto.SettlementPush) (<-chan *tbproto.SettleAck, bool)
}

// Deps lists every collaborator broker.Open requires.
type Deps struct {
	Logger     zerolog.Logger
	Now        func() time.Time
	Registry   RegistryService
	Ledger     LedgerService
	Admission  AdmissionService
	Reputation ReputationService
	Pusher     PushService
	Pricing    *PriceTable
	TrackerKey ed25519.PrivateKey
}

// fallbackReputation is the v1 default when no reputation subsystem is wired.
type fallbackReputation struct{}

func (fallbackReputation) Score(ids.IdentityID) (float64, bool)      { return 0.5, false }
func (fallbackReputation) IsFrozen(ids.IdentityID) bool              { return false }
func (fallbackReputation) RecordOfferOutcome(ids.IdentityID, string) {}

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
// RecordOfferOutcome's error return mirrors *reputation.Subsystem; the
// broker discards it (the tunnel decision must not depend on a
// reputation hiccup).
type ReputationService interface {
	Score(ids.IdentityID) (float64, bool)
	IsFrozen(ids.IdentityID) bool
	RecordOfferOutcome(seeder ids.IdentityID, outcome string) error
	OnLedgerEvent(ev admission.LedgerEvent)
}

// PushService is the slice of *server.Server the broker uses for offer +
// settlement push streams.
type PushService interface {
	PushOfferTo(ids.IdentityID, *tbproto.OfferPush) (<-chan *tbproto.OfferDecision, bool)
	PushSettlementTo(ids.IdentityID, *tbproto.SettlementPush) (<-chan *tbproto.SettleAck, bool)
}

// IdentityResolver maps an IdentityID to its Ed25519 public key.
// Settlement uses it to verify the consumer's counter-signature before
// appending a USAGE entry to the ledger. Returns ok=false when the peer
// is not currently connected; the broker falls through to
// ConsumerSigMissing=true in that case.
//
// v1: settlement does not yet perform sig verification (T17.5 deferred;
// see the TODO in awaitSettle). The interface is wired now so the
// infrastructure is in place for a future wire-format amendment.
type IdentityResolver interface {
	PeerPubkey(id ids.IdentityID) (ed25519.PublicKey, bool)
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
	// Identity resolves consumer IdentityID → Ed25519 pubkey from the live
	// mTLS connection table. Optional in v1: nil falls through to
	// ConsumerSigMissing=true on every settlement.
	Identity IdentityResolver
}

// fallbackReputation is the v1 default when no reputation subsystem is wired.
type fallbackReputation struct{}

func (fallbackReputation) Score(ids.IdentityID) (float64, bool)            { return 0.5, false }
func (fallbackReputation) IsFrozen(ids.IdentityID) bool                    { return false }
func (fallbackReputation) RecordOfferOutcome(ids.IdentityID, string) error { return nil }
func (fallbackReputation) OnLedgerEvent(admission.LedgerEvent)             {}

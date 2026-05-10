package reputation

// Role classifies which population an event contributes to. Spec §3.1
// (consumer signals) vs §3.2 (seeder signals). RoleAgnostic is reserved
// for events that don't bucket cleanly (e.g. a manual operator action).
type Role uint8

const (
	RoleConsumer Role = 0
	RoleSeeder   Role = 1
	RoleAgnostic Role = 2
)

func (r Role) String() string {
	switch r {
	case RoleConsumer:
		return "consumer"
	case RoleSeeder:
		return "seeder"
	case RoleAgnostic:
		return "agnostic"
	default:
		return "unknown"
	}
}

// SignalKind enumerates rep_events.event_type values. Stable on disk.
type SignalKind uint16

const (
	SignalUnspecified SignalKind = 0

	// Consumer-side primary signals (§6.1).
	SignalBrokerRequest   SignalKind = 1
	SignalProofRejection  SignalKind = 2
	SignalExhaustionClaim SignalKind = 3

	// Seeder-side primary signals (§6.1).
	SignalCostReportDeviation SignalKind = 4
	SignalConsumerComplaint   SignalKind = 5

	// Seeder-side secondary signals.
	SignalOfferAccept      SignalKind = 6
	SignalOfferReject      SignalKind = 7
	SignalOfferUnreachable SignalKind = 8

	// Settlement-derived secondary signals.
	SignalSettlementClean      SignalKind = 9
	SignalSettlementSigMissing SignalKind = 10

	// Dispute secondary signals.
	SignalDisputeFiled  SignalKind = 11
	SignalDisputeUpheld SignalKind = 12

	// proof_fidelity_level (§3.1): per-broker_request classifier,
	// emitted by the api/ verification path. value ∈ {1.0, 0.5, 0.0}
	// for full_two_signal / partial / degraded.
	SignalProofFidelityLevel SignalKind = 13
)

// Role returns the population this signal belongs to.
func (s SignalKind) Role() Role {
	switch s {
	case SignalBrokerRequest, SignalProofRejection, SignalExhaustionClaim,
		SignalDisputeFiled, SignalDisputeUpheld, SignalProofFidelityLevel:
		return RoleConsumer
	case SignalCostReportDeviation, SignalConsumerComplaint,
		SignalOfferAccept, SignalOfferReject, SignalOfferUnreachable,
		SignalSettlementClean, SignalSettlementSigMissing:
		return RoleSeeder
	default:
		return RoleAgnostic
	}
}

// IsPrimary returns true iff this signal participates in §6.1 z-score
// outlier detection.
func (s SignalKind) IsPrimary() bool {
	switch s {
	case SignalBrokerRequest, SignalProofRejection, SignalExhaustionClaim,
		SignalCostReportDeviation, SignalConsumerComplaint:
		return true
	default:
		return false
	}
}

// PrimarySignals returns the static list of primary signals in
// evaluation order. The evaluator iterates this list once per cycle.
func PrimarySignals() []SignalKind {
	return []SignalKind{
		SignalBrokerRequest,
		SignalProofRejection,
		SignalExhaustionClaim,
		SignalCostReportDeviation,
		SignalConsumerComplaint,
	}
}

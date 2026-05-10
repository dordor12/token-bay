package reputation

// BreachKind enumerates the categorical breaches from spec §6.2. Each
// kind maps to an immediate state action. Stable on disk
// (rep_state.reasons.breach_kind).
type BreachKind uint8

const (
	BreachUnspecified             BreachKind = 0
	BreachInvalidProofSignature   BreachKind = 1
	BreachInconsistentSeederSig   BreachKind = 2
	BreachReplayConfirmedNonce    BreachKind = 3
	BreachConsecutiveProofRejects BreachKind = 4
)

// String returns the on-disk label for rep_state.reasons.breach_kind.
func (b BreachKind) String() string {
	switch b {
	case BreachInvalidProofSignature:
		return "invalid_proof_signature"
	case BreachInconsistentSeederSig:
		return "inconsistent_seeder_sig"
	case BreachReplayConfirmedNonce:
		return "replay_confirmed_nonce"
	case BreachConsecutiveProofRejects:
		return "consecutive_proof_rejects"
	default:
		return "unspecified"
	}
}

// ImmediateAction returns the state the identity moves to on this
// breach. All v1 breaches transition to AUDIT; severe breaches that go
// straight to FROZEN are reserved for v2.
func (b BreachKind) ImmediateAction() State {
	switch b {
	case BreachInvalidProofSignature,
		BreachInconsistentSeederSig,
		BreachReplayConfirmedNonce,
		BreachConsecutiveProofRejects:
		return StateAudit
	default:
		return StateOK
	}
}

// signalRole returns the role bucket the audit-trail row for this
// breach should be filed under. v1 breaches are all consumer-side
// (proof + replay) or seeder-side (seeder sig). Used by
// RecordCategoricalBreach in ingest.go.
func (b BreachKind) signalRole() Role {
	switch b {
	case BreachInconsistentSeederSig:
		return RoleSeeder
	case BreachInvalidProofSignature, BreachReplayConfirmedNonce,
		BreachConsecutiveProofRejects:
		return RoleConsumer
	default:
		return RoleAgnostic
	}
}

package reputation

import "time"

// State is the reputation state of an identity. Spec §4.
type State uint8

const (
	StateOK     State = 0
	StateAudit  State = 1
	StateFrozen State = 2
)

// String returns the operator-facing label.
func (s State) String() string {
	switch s {
	case StateOK:
		return "OK"
	case StateAudit:
		return "AUDIT"
	case StateFrozen:
		return "FROZEN"
	default:
		return "UNKNOWN"
	}
}

// ReputationStatus is the public Status() return shape. Reasons is a
// slice copy of rep_state.reasons (most-recent last). Empty for
// identities never seen.
//
// The type is named ReputationStatus (not Status) to match the spec §2.3
// signature verbatim and avoid ambiguity at the call site; the revive
// stutter lint is suppressed below.
type ReputationStatus struct { //nolint:revive // name matches spec §2.3; stutter is intentional
	State   State
	Since   time.Time
	Reasons []ReasonRecord
}

// ReasonRecord is one element of rep_state.reasons. JSON tags are the
// on-disk field names.
type ReasonRecord struct {
	Kind       string  `json:"kind"` // "zscore" | "breach" | "manual" | "transition"
	Signal     string  `json:"signal,omitempty"`
	BreachKind string  `json:"breach_kind,omitempty"`
	Z          float64 `json:"z,omitempty"`
	Window     string  `json:"window,omitempty"`
	Operator   string  `json:"operator,omitempty"`
	At         int64   `json:"at"`
}

// canTransition reports whether the evaluator or RecordCategoricalBreach
// is allowed to move from `from` to `to`. FROZEN is terminal in both
// paths; only manual operator action (deferred) clears it.
func canTransition(from, to State) bool {
	if from == to {
		return false
	}
	if from == StateFrozen {
		return false
	}
	return true
}

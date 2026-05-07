package broker

import "errors"

// Sentinel errors returned by exported broker methods. Lifecycle errors
// (insufficient-credits, duplicate-reservation, illegal-transition,
// unknown-request) live in tracker/internal/session.
var (
	ErrUnknownReservation = errors.New("broker: unknown reservation")
	ErrUnknownModel       = errors.New("broker: unknown model")
	ErrSeederMismatch     = errors.New("broker: usage_report seeder mismatch")
	ErrModelMismatch      = errors.New("broker: usage_report model mismatch")
	ErrCostOverspend      = errors.New("broker: usage_report cost overspend")
	ErrSeederSigInvalid   = errors.New("broker: usage_report seeder signature invalid")
	ErrDuplicateSettle    = errors.New("broker: duplicate settle for preimage")
	ErrUnknownPreimage    = errors.New("broker: unknown preimage hash")
)

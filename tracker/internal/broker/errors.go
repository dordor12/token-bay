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
	// ErrIdentityFrozen is returned by Submit when the consumer's
	// identity is present in the federation revocation archive — i.e.
	// a peer tracker has FROZEN the identity and gossiped the
	// REVOCATION here. The api/ layer maps this to ErrFrozen /
	// RPC_STATUS_FROZEN. Reputation §12 acceptance: "Frozen identity's
	// broker_request returns IDENTITY_FROZEN."
	ErrIdentityFrozen = errors.New("broker: identity frozen by peer revocation")
)

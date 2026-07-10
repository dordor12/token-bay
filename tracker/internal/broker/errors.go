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
	// ErrDuplicateUsageReport is returned by HandleUsageReport when a
	// settlement for the request_id is already in flight. The first report
	// creates the pending entry; any later report for the same request_id is
	// a replay (benign seeder retry after a lost UsageAck, or malicious) and
	// must not spawn a second settlement/append — participant sigs are over
	// the sequencing-independent usage-assertion, so a second append would
	// double-debit the consumer. A report arriving after settlement completed
	// is caught by the state guard (ErrInvalidState) instead. Maps to
	// DUPLICATE_USAGE_REPORT in api/.
	ErrDuplicateUsageReport = errors.New("broker: duplicate usage_report for request")
	// ErrInvalidState is returned by HandleUsageReport when the in-flight
	// request is not in ASSIGNED or SERVING — settling a request that has
	// not been assigned (or has already terminated) is a protocol error.
	// Maps to RPC_STATUS_INVALID code INVALID_STATE in api/. spec §5.2 step 2.
	ErrInvalidState = errors.New("broker: usage_report invalid request state")
	// ErrConsumerSig is returned by HandleSettle when the consumer's
	// counter-signature fails verification against the preimage body.
	// No ledger entry is written on this path. spec §5.2.
	ErrConsumerSig = errors.New("broker: consumer signature invalid")
	// ErrIdentityFrozen is returned by Submit when the consumer's
	// identity is present in the federation revocation archive — i.e.
	// a peer tracker has FROZEN the identity and gossiped the
	// REVOCATION here. The api/ layer maps this to ErrFrozen /
	// RPC_STATUS_FROZEN. Reputation §12 acceptance: "Frozen identity's
	// broker_request returns IDENTITY_FROZEN."
	ErrIdentityFrozen = errors.New("broker: identity frozen by peer revocation")
)

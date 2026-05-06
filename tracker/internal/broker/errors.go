package broker

import "errors"

// Sentinel errors returned by exported broker methods.
var (
	ErrInsufficientCredits   = errors.New("broker: insufficient credits")
	ErrDuplicateReservation  = errors.New("broker: duplicate reservation")
	ErrUnknownReservation    = errors.New("broker: unknown reservation")
	ErrUnknownModel          = errors.New("broker: unknown model")
	ErrIllegalTransition     = errors.New("broker: illegal state transition")
	ErrUnknownRequest        = errors.New("broker: unknown request")
	ErrSeederMismatch        = errors.New("broker: usage_report seeder mismatch")
	ErrModelMismatch         = errors.New("broker: usage_report model mismatch")
	ErrCostOverspend         = errors.New("broker: usage_report cost overspend")
	ErrSeederSigInvalid      = errors.New("broker: usage_report seeder signature invalid")
	ErrDuplicateSettle       = errors.New("broker: duplicate settle for preimage")
	ErrUnknownPreimage       = errors.New("broker: unknown preimage hash")
)

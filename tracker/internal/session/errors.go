package session

import "errors"

// Sentinel errors returned by exported session methods.
var (
	ErrInsufficientCredits  = errors.New("session: insufficient credits")
	ErrDuplicateReservation = errors.New("session: duplicate reservation")
	ErrIllegalTransition    = errors.New("session: illegal state transition")
	ErrUnknownRequest       = errors.New("session: unknown request")
)

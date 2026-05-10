package seederflow

import "errors"

// Sentinel errors returned by Coordinator.
var (
	// ErrInvalidConfig is returned by New when a required Config field
	// is missing or invalid.
	ErrInvalidConfig = errors.New("seederflow: invalid config")

	// ErrAlreadyStarted is returned by Run on the second call.
	ErrAlreadyStarted = errors.New("seederflow: already started")

	// ErrClosed is returned by Run after Close.
	ErrClosed = errors.New("seederflow: closed")

	// ErrNoReservation is returned by Serve when the inbound consumer
	// did not match any registered reservation (envelope hash unknown,
	// or already consumed).
	ErrNoReservation = errors.New("seederflow: no matching reservation")

	// ErrNotAdvertising is returned by HandleOffer when current state
	// blocks advertising (idle window closed, activity grace not
	// elapsed, recent rate-limit, or conformance failed).
	ErrNotAdvertising = errors.New("seederflow: not currently advertising")
)

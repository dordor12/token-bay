// Package session owns the per-request lifecycle state shared by
// tracker/internal/broker's selection and settlement subsystems:
// the Inflight state machine, the in-memory credit Reservations
// ledger, and the typed expiry sweep helpers driven by the broker's
// reservation-TTL reaper.
//
// session is a pure state package. It does not import internal/registry,
// internal/ledger, internal/admission, or any wire type beyond the
// EnvelopeBody it carries as request metadata. Goroutine ownership
// (timers, tickers) lives in broker; session exposes synchronous
// helpers only.
//
// See docs/superpowers/specs/tracker/2026-05-07-tracker-session-design.md.
package session

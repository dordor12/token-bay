package stunturn

import "errors"

// ErrInvalidConfig is returned by NewAllocator when AllocatorConfig is
// malformed. Wrap-friendly: callers wrap a specific reason with %w.
var ErrInvalidConfig = errors.New("stunturn: invalid allocator config")

// ErrInvalidPacket is returned by DecodeBindingRequest when the bytes
// are not a valid RFC 5389 binding request (malformed bytes, wrong
// message type, wrong magic cookie, or unknown comprehension-required
// attribute).
var ErrInvalidPacket = errors.New("stunturn: invalid stun packet")

// ErrUnknownToken is returned by ResolveAndCharge when the token is not
// in the allocator (never allocated, or already Released / Swept).
var ErrUnknownToken = errors.New("stunturn: unknown session token")

// ErrSessionExpired is returned by ResolveAndCharge when the session's
// LastActive is older than SessionTTL. The entry is deleted on the same
// call; subsequent calls return ErrUnknownToken.
var ErrSessionExpired = errors.New("stunturn: session expired")

// ErrThrottled is returned by ResolveAndCharge / Charge when the
// seeder's per-second bandwidth bucket has insufficient credit for the
// requested byte count. The bucket is NOT debited on a throttled call.
var ErrThrottled = errors.New("stunturn: seeder throttled")

// ErrDuplicateRequest is returned by Allocate when a session for the
// same RequestID is already live. Caller should treat the prior
// allocation as still good and not retry.
var ErrDuplicateRequest = errors.New("stunturn: duplicate request id")

// ErrRandFailed wraps any failure from the injected io.Reader during
// token generation. The error chain preserves the underlying error via
// errors.Is / errors.As.
var ErrRandFailed = errors.New("stunturn: token randomness failed")

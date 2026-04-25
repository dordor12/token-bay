package registry

import "errors"

// ErrUnknownSeeder is returned when an operation references an IdentityID that
// is not currently in the registry.
var ErrUnknownSeeder = errors.New("registry: unknown seeder")

// ErrInvalidShardCount is returned by New when numShards is not a positive int.
var ErrInvalidShardCount = errors.New("registry: shard count must be positive")

// ErrInvalidHeadroom is returned when a headroom value is outside [0.0, 1.0].
var ErrInvalidHeadroom = errors.New("registry: headroom must be within [0.0, 1.0]")

// ErrLoadUnderflow is returned by DecLoad when the seeder's load is already 0
// (catching a double-decrement bug at the call site rather than silently
// clamping).
var ErrLoadUnderflow = errors.New("registry: load underflow")

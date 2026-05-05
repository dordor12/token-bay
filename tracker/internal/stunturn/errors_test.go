package stunturn

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSentinelsAreDistinct asserts every pair of sentinel errors is
// distinguishable via errors.Is.
func TestSentinelsAreDistinct(t *testing.T) {
	all := []error{
		ErrInvalidConfig,
		ErrInvalidPacket,
		ErrUnknownToken,
		ErrSessionExpired,
		ErrThrottled,
		ErrDuplicateRequest,
		ErrRandFailed,
	}
	for i, a := range all {
		for j, b := range all {
			if i == j {
				continue
			}
			assert.False(t, errors.Is(a, b),
				"errors.Is(%v, %v) must be false (sentinels must be distinct)", a, b)
		}
	}
}

// TestErrInvalidConfig_WrapsReason asserts that wrapping ErrInvalidConfig
// with %w preserves both the sentinel identity (errors.Is) and the underlying
// reason text.
func TestErrInvalidConfig_WrapsReason(t *testing.T) {
	wrapped := fmt.Errorf("%w: MaxKbpsPerSeeder must be > 0", ErrInvalidConfig)

	require.ErrorIs(t, wrapped, ErrInvalidConfig)
	assert.Contains(t, wrapped.Error(), "MaxKbpsPerSeeder must be > 0")
}

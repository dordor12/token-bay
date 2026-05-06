package tunnel

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSentinelsAreDistinct(t *testing.T) {
	all := []error{
		ErrInvalidConfig,
		ErrPeerPinMismatch,
		ErrALPNMismatch,
		ErrHandshakeFailed,
		ErrFramingViolation,
		ErrRequestTooLarge,
		ErrPeerError,
		ErrTunnelClosed,
	}
	seen := make(map[string]bool, len(all))
	for _, e := range all {
		assert.NotNil(t, e)
		msg := e.Error()
		assert.False(t, seen[msg], "duplicate sentinel message: %q", msg)
		seen[msg] = true
	}
}

func TestErrorsIsWrapped(t *testing.T) {
	wrapped := fmt.Errorf("dial: %w", ErrPeerPinMismatch)
	assert.True(t, errors.Is(wrapped, ErrPeerPinMismatch))
	assert.False(t, errors.Is(wrapped, ErrALPNMismatch))
}

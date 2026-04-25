//go:build windows

package ratelimit

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClaudePTYProbeRunner_WindowsStub(t *testing.T) {
	r := NewClaudePTYProbeRunner()
	data, err := r.Probe(context.Background())
	assert.Nil(t, data)
	assert.True(t, errors.Is(err, ErrUnsupportedPlatform),
		"expected ErrUnsupportedPlatform, got %v", err)
}

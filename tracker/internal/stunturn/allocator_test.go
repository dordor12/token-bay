package stunturn

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validCfg returns an AllocatorConfig that NewAllocator accepts. Subtests
// mutate one field and assert the precise validation error.
func validCfg() AllocatorConfig {
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	return AllocatorConfig{
		MaxKbpsPerSeeder: 1024,
		SessionTTL:       30 * time.Second,
		Now:              func() time.Time { return now },
		Rand:             rand.Reader,
	}
}

func TestNewAllocator_Valid(t *testing.T) {
	a, err := NewAllocator(validCfg())

	require.NoError(t, err)
	require.NotNil(t, a)
}

func TestNewAllocator_RejectsZeroKbps(t *testing.T) {
	cfg := validCfg()
	cfg.MaxKbpsPerSeeder = 0

	a, err := NewAllocator(cfg)

	assert.Nil(t, a)
	require.True(t, errors.Is(err, ErrInvalidConfig), "want ErrInvalidConfig, got %v", err)
	assert.True(t, strings.Contains(err.Error(), "MaxKbpsPerSeeder"), "error should name the field, got %q", err.Error())
}

func TestNewAllocator_RejectsNegativeKbps(t *testing.T) {
	cfg := validCfg()
	cfg.MaxKbpsPerSeeder = -1

	a, err := NewAllocator(cfg)

	assert.Nil(t, a)
	require.True(t, errors.Is(err, ErrInvalidConfig))
	assert.True(t, strings.Contains(err.Error(), "MaxKbpsPerSeeder"))
}

func TestNewAllocator_RejectsZeroTTL(t *testing.T) {
	cfg := validCfg()
	cfg.SessionTTL = 0

	a, err := NewAllocator(cfg)

	assert.Nil(t, a)
	require.True(t, errors.Is(err, ErrInvalidConfig))
	assert.True(t, strings.Contains(err.Error(), "SessionTTL"))
}

func TestNewAllocator_RejectsNilNow(t *testing.T) {
	cfg := validCfg()
	cfg.Now = nil

	a, err := NewAllocator(cfg)

	assert.Nil(t, a)
	require.True(t, errors.Is(err, ErrInvalidConfig))
	assert.True(t, strings.Contains(err.Error(), "Now"))
}

func TestNewAllocator_RejectsNilRand(t *testing.T) {
	cfg := validCfg()
	cfg.Rand = nil

	a, err := NewAllocator(cfg)

	assert.Nil(t, a)
	require.True(t, errors.Is(err, ErrInvalidConfig))
	assert.True(t, strings.Contains(err.Error(), "Rand"))
}

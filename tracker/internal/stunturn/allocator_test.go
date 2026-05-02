package stunturn

import (
	"bytes"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/token-bay/token-bay/shared/ids"
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

// fixedClockCfg returns a cfg whose Now and Rand are deterministic.
func fixedClockCfg(now time.Time, randBytes []byte) AllocatorConfig {
	return AllocatorConfig{
		MaxKbpsPerSeeder: 1024,
		SessionTTL:       30 * time.Second,
		Now:              func() time.Time { return now },
		Rand:             bytes.NewReader(randBytes),
	}
}

func mustAlloc(t *testing.T, a *Allocator, consumer, seeder ids.IdentityID, reqID [16]byte, now time.Time) Session {
	t.Helper()
	s, err := a.Allocate(consumer, seeder, reqID, now)
	require.NoError(t, err)
	return s
}

// id8 returns an IdentityID whose first byte is b (rest zero) — convenient
// for distinguishable test identities.
func id8(b byte) ids.IdentityID {
	var raw [32]byte
	raw[0] = b
	return ids.IdentityID(raw)
}

// req8 returns a [16]byte RequestID with first byte b.
func req8(b byte) [16]byte {
	var r [16]byte
	r[0] = b
	return r
}

func TestAllocate_HappyPath(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1) // 0x01..0x10
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)

	s, err := a.Allocate(id8(1), id8(2), req8(7), t0)

	require.NoError(t, err)
	assert.Equal(t, uint64(1), s.SessionID)
	assert.Equal(t, id8(1), s.ConsumerID)
	assert.Equal(t, id8(2), s.SeederID)
	assert.Equal(t, req8(7), s.RequestID)
	assert.Equal(t, t0, s.AllocatedAt)
	assert.Equal(t, t0, s.LastActive)
	wantTok := Token{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	assert.Equal(t, wantTok, s.Token)
}

func TestAllocate_AssignsDistinctSessionIDs(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	// 48 bytes = 3 distinct tokens
	tokBytes := make([]byte, 48)
	for i := range tokBytes {
		tokBytes[i] = byte(i)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)

	s1 := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)
	s2 := mustAlloc(t, a, id8(1), id8(2), req8(2), t0)
	s3 := mustAlloc(t, a, id8(1), id8(2), req8(3), t0)

	assert.Equal(t, uint64(1), s1.SessionID)
	assert.Equal(t, uint64(2), s2.SessionID)
	assert.Equal(t, uint64(3), s3.SessionID)
	assert.NotEqual(t, s1.Token, s2.Token)
	assert.NotEqual(t, s2.Token, s3.Token)
}

func TestAllocate_DuplicateRequestID(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 32) // enough for two attempts
	for i := range tokBytes {
		tokBytes[i] = byte(i)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	s1 := mustAlloc(t, a, id8(1), id8(2), req8(7), t0)

	s2, err2 := a.Allocate(id8(1), id8(2), req8(7), t0)

	require.True(t, errors.Is(err2, ErrDuplicateRequest), "want ErrDuplicateRequest, got %v", err2)
	assert.Equal(t, Session{}, s2)
	// Original is unchanged.
	assert.Equal(t, uint64(1), s1.SessionID)
}

// errReader always errors. Used to simulate Rand failure.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("simulated rand failure") }

func TestAllocate_RandFailure(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	cfg := AllocatorConfig{
		MaxKbpsPerSeeder: 1024,
		SessionTTL:       30 * time.Second,
		Now:              func() time.Time { return t0 },
		Rand:             errReader{},
	}
	a, err := NewAllocator(cfg)
	require.NoError(t, err)

	s, err := a.Allocate(id8(1), id8(2), req8(1), t0)

	assert.Equal(t, Session{}, s)
	require.True(t, errors.Is(err, ErrRandFailed), "want ErrRandFailed, got %v", err)
	assert.Contains(t, err.Error(), "simulated rand failure")
}

func TestResolve_Hit(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	want := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	got, ok := a.Resolve(want.Token, t0)

	require.True(t, ok)
	assert.Equal(t, want, got)
}

func TestResolve_UnknownToken(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	a, err := NewAllocator(fixedClockCfg(t0, make([]byte, 16)))
	require.NoError(t, err)

	got, ok := a.Resolve(Token{0xff}, t0)

	assert.False(t, ok)
	assert.Equal(t, Session{}, got)
}

// TestResolve_DoesNotUpdateLastActive: Resolve is a peek only; it
// must not extend the session's life. Session expires at LastActive +
// SessionTTL regardless of Resolve calls.
func TestResolve_DoesNotUpdateLastActive(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	want := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	// Resolve at a later time.
	later := t0.Add(10 * time.Second)
	got, ok := a.Resolve(want.Token, later)

	require.True(t, ok)
	assert.Equal(t, t0, got.LastActive,
		"Resolve must not advance LastActive (peek-only contract)")
}

// TestResolve_ReturnsCopy: mutating the returned Session must not
// change subsequent Resolve results.
func TestResolve_ReturnsCopy(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	want := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	first, _ := a.Resolve(want.Token, t0)
	first.LastActive = first.LastActive.Add(1000 * time.Hour) // mutate caller copy

	second, ok := a.Resolve(want.Token, t0)
	require.True(t, ok)
	assert.Equal(t, t0, second.LastActive,
		"allocator state must not be mutable through a returned Session")
}

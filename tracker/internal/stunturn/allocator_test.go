package stunturn

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"strings"
	"sync"
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

func TestResolveAndCharge_Happy(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	want := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	got, err := a.ResolveAndCharge(want.Token, 1500, t0)

	require.NoError(t, err)
	assert.Equal(t, want.SessionID, got.SessionID)
	assert.Equal(t, t0, got.LastActive)
}

func TestResolveAndCharge_UnknownToken(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	a, err := NewAllocator(fixedClockCfg(t0, make([]byte, 16)))
	require.NoError(t, err)

	_, err = a.ResolveAndCharge(Token{0xff}, 1500, t0)

	require.True(t, errors.Is(err, ErrUnknownToken), "want ErrUnknownToken, got %v", err)
}

// TestResolveAndCharge_Throttled: at MaxKbpsPerSeeder=1024, capacity
// is 1024*1024/8 = 131072 bytes. Charge 200_000 fails; bucket is NOT
// debited so a follow-up small charge succeeds.
func TestResolveAndCharge_Throttled(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	want := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	_, err = a.ResolveAndCharge(want.Token, 200_000, t0)
	require.True(t, errors.Is(err, ErrThrottled))

	// Bucket was not debited — a small charge still succeeds.
	_, err = a.ResolveAndCharge(want.Token, 100, t0)
	require.NoError(t, err)
}

// TestResolveAndCharge_RefillsOverTime: exhaust the bucket, advance
// 0.5s, half-capacity should be available.
func TestResolveAndCharge_RefillsOverTime(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	want := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	// 131072 = capacity; drain it.
	_, err = a.ResolveAndCharge(want.Token, 131072, t0)
	require.NoError(t, err)
	// Same instant — fully drained.
	_, err = a.ResolveAndCharge(want.Token, 1, t0)
	require.True(t, errors.Is(err, ErrThrottled))

	// Half a second later — half capacity (65536) refilled.
	half := t0.Add(500 * time.Millisecond)
	_, err = a.ResolveAndCharge(want.Token, 65000, half)
	require.NoError(t, err)
	_, err = a.ResolveAndCharge(want.Token, 1000, half)
	require.True(t, errors.Is(err, ErrThrottled), "remaining bucket should be ~536 bytes; 1000 must throttle")
}

// TestResolveAndCharge_ExpiresOnIdle: SessionTTL=30s; advance 31s
// without activity; first call returns ErrSessionExpired (and deletes
// the entry); follow-up returns ErrUnknownToken.
func TestResolveAndCharge_ExpiresOnIdle(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	want := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	expired := t0.Add(31 * time.Second)
	_, err = a.ResolveAndCharge(want.Token, 100, expired)
	require.True(t, errors.Is(err, ErrSessionExpired))

	_, err = a.ResolveAndCharge(want.Token, 100, expired)
	require.True(t, errors.Is(err, ErrUnknownToken),
		"after ErrSessionExpired the entry must be deleted")
}

// TestResolveAndCharge_LastActiveAdvances: a session that gets touched
// every 10s does not expire at the 30s mark.
func TestResolveAndCharge_LastActiveAdvances(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	want := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	for i := 1; i <= 4; i++ {
		_, err := a.ResolveAndCharge(want.Token, 100, t0.Add(time.Duration(i*10)*time.Second))
		require.NoError(t, err, "call %d at +%ds must succeed", i, i*10)
	}
}

// TestResolveAndCharge_ClockBackwards: a clock that goes backward must
// not produce negative refill (no underflow).
func TestResolveAndCharge_ClockBackwards(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	want := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	// Drain the bucket at t0.
	_, err = a.ResolveAndCharge(want.Token, 131072, t0)
	require.NoError(t, err)

	// Time goes backwards; available must remain 0 (no negative refill,
	// no overflow). The 1-byte ask still throttles.
	_, err = a.ResolveAndCharge(want.Token, 1, t0.Add(-1*time.Second))
	require.True(t, errors.Is(err, ErrThrottled))
}

// TestResolveAndCharge_NoBytes_KeepsAlive: charging 0 bytes (or
// negative) does not throttle and DOES update LastActive — the listener
// observed a packet, the session is alive.
func TestResolveAndCharge_NoBytes_KeepsAlive(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	want := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	// Drain the bucket so any debit would throttle.
	_, err = a.ResolveAndCharge(want.Token, 131072, t0)
	require.NoError(t, err)

	got, err := a.ResolveAndCharge(want.Token, 0, t0.Add(20*time.Second))
	require.NoError(t, err, "n=0 should never throttle")
	assert.Equal(t, t0.Add(20*time.Second), got.LastActive)
}

func TestCharge_Happy(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	a, err := NewAllocator(fixedClockCfg(t0, make([]byte, 16)))
	require.NoError(t, err)

	require.NoError(t, a.Charge(id8(2), 1000, t0))
}

func TestCharge_ZeroAndNegativeAreNoOps(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	a, err := NewAllocator(fixedClockCfg(t0, make([]byte, 16)))
	require.NoError(t, err)

	// Drain bucket via a real Charge.
	require.NoError(t, a.Charge(id8(2), 131072, t0))

	// 0 and negative must not return ErrThrottled even though bucket is empty.
	require.NoError(t, a.Charge(id8(2), 0, t0))
	require.NoError(t, a.Charge(id8(2), -1, t0))
}

func TestCharge_LazyBucketInitForUnknownSeeder(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	a, err := NewAllocator(fixedClockCfg(t0, make([]byte, 16)))
	require.NoError(t, err)

	// Seeder never had Allocate called; Charge still works.
	require.NoError(t, a.Charge(id8(99), 1000, t0))
}

func TestCharge_Throttled(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	a, err := NewAllocator(fixedClockCfg(t0, make([]byte, 16)))
	require.NoError(t, err)

	require.NoError(t, a.Charge(id8(2), 131072, t0)) // drain
	err = a.Charge(id8(2), 1, t0)
	require.True(t, errors.Is(err, ErrThrottled), "want ErrThrottled, got %v", err)
}

func TestRelease_RemovesAllIndexes(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	s := mustAlloc(t, a, id8(1), id8(2), req8(7), t0)

	a.Release(s.SessionID)

	// All three indexes report unknown.
	_, ok := a.Resolve(s.Token, t0)
	assert.False(t, ok, "byToken should be empty")
	_, err = a.ResolveAndCharge(s.Token, 100, t0)
	assert.True(t, errors.Is(err, ErrUnknownToken))

	// byReq is freed too — we can re-Allocate the same RequestID.
	tokBytes2 := []byte{
		0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00, 0x11,
		0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99,
	}
	cfg := fixedClockCfg(t0, tokBytes2)
	a2, err := NewAllocator(cfg)
	require.NoError(t, err)
	s1 := mustAlloc(t, a2, id8(1), id8(2), req8(7), t0)
	a2.Release(s1.SessionID)
	// Allocate again with the same RequestID; must not return
	// ErrDuplicateRequest.
	cfg.Rand = bytes.NewReader([]byte{
		0x10, 0x20, 0x30, 0x40, 0x50, 0x60, 0x70, 0x80,
		0x90, 0xa0, 0xb0, 0xc0, 0xd0, 0xe0, 0xf0, 0x01,
	})
	a3, err := NewAllocator(cfg)
	require.NoError(t, err)
	_, err = a3.Allocate(id8(1), id8(2), req8(7), t0)
	require.NoError(t, err)
}

func TestRelease_UnknownSID_NoPanic(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	a, err := NewAllocator(fixedClockCfg(t0, make([]byte, 16)))
	require.NoError(t, err)

	assert.NotPanics(t, func() { a.Release(0) })
	assert.NotPanics(t, func() { a.Release(99999) })
}

func TestRelease_Idempotent(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	s := mustAlloc(t, a, id8(1), id8(2), req8(7), t0)

	a.Release(s.SessionID)
	assert.NotPanics(t, func() { a.Release(s.SessionID) })
}

func TestSweep_Empty(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	a, err := NewAllocator(fixedClockCfg(t0, make([]byte, 16)))
	require.NoError(t, err)

	assert.Equal(t, 0, a.Sweep(t0))
}

func TestSweep_RemovesIdleSessions(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 32) // two tokens
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	s1 := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)
	s2 := mustAlloc(t, a, id8(1), id8(2), req8(2), t0)

	swept := a.Sweep(t0.Add(31 * time.Second))

	assert.Equal(t, 2, swept)
	_, ok := a.Resolve(s1.Token, t0)
	assert.False(t, ok)
	_, ok = a.Resolve(s2.Token, t0)
	assert.False(t, ok)
}

func TestSweep_KeepsActiveSessions(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	s := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	// Touch the session at +15s.
	_, err = a.ResolveAndCharge(s.Token, 100, t0.Add(15*time.Second))
	require.NoError(t, err)

	// Sweep at +31s — LastActive is +15s, so age is 16s < 30s TTL: keep.
	swept := a.Sweep(t0.Add(31 * time.Second))
	assert.Equal(t, 0, swept)
	_, ok := a.Resolve(s.Token, t0)
	assert.True(t, ok)
}

func TestSweep_DoesNotTouchBuckets(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	tokBytes := make([]byte, 16)
	for i := range tokBytes {
		tokBytes[i] = byte(i + 1)
	}
	a, err := NewAllocator(fixedClockCfg(t0, tokBytes))
	require.NoError(t, err)
	s := mustAlloc(t, a, id8(1), id8(2), req8(1), t0)

	// Drain the bucket so we can detect bucket re-init.
	_, err = a.ResolveAndCharge(s.Token, 131072, t0)
	require.NoError(t, err)

	// Sweep removes the session.
	require.Equal(t, 1, a.Sweep(t0.Add(31*time.Second)))

	// The bucket survived. Charge at the time of drain verifies the bucket
	// persists in its drained state (would succeed if Sweep had wiped
	// buckets and re-init filled it).
	err = a.Charge(id8(2), 1, t0)
	require.True(t, errors.Is(err, ErrThrottled),
		"bucket should persist post-Sweep; got %v", err)
}

// TestConcurrent_AllocateResolveCharge fans out N goroutines that each
// allocate, hit the session with K ResolveAndCharge calls, then release.
// Race detector must stay clean and the final state must be empty.
func TestConcurrent_AllocateResolveCharge(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	const N, K = 16, 32
	cfg := AllocatorConfig{
		MaxKbpsPerSeeder: 1024,
		SessionTTL:       30 * time.Second,
		Now:              func() time.Time { return t0 },
		Rand:             rand.Reader, // real randomness for unique tokens
	}
	a, err := NewAllocator(cfg)
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			var reqID [16]byte
			binary.BigEndian.PutUint64(reqID[:], uint64(i))
			s, err := a.Allocate(id8(byte(i+1)), id8(byte(i+1)), reqID, t0)
			if err != nil {
				t.Errorf("Allocate: %v", err)
				return
			}
			for j := 0; j < K; j++ {
				if _, err := a.ResolveAndCharge(s.Token, 100, t0); err != nil {
					t.Errorf("RAC: %v", err)
					return
				}
			}
			a.Release(s.SessionID)
		}()
	}
	wg.Wait()

	// Final state: zero sessions left.
	assert.Equal(t, 0, a.Sweep(t0.Add(time.Hour)),
		"all sessions should already be Released; Sweep finds nothing")
}

// TestConcurrent_DuplicateRequestRace: N goroutines race to Allocate
// the same RequestID. Exactly one wins; the rest see ErrDuplicateRequest.
func TestConcurrent_DuplicateRequestRace(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	const N = 32
	cfg := AllocatorConfig{
		MaxKbpsPerSeeder: 1024,
		SessionTTL:       30 * time.Second,
		Now:              func() time.Time { return t0 },
		Rand:             rand.Reader,
	}
	a, err := NewAllocator(cfg)
	require.NoError(t, err)
	reqID := req8(42)

	var wg sync.WaitGroup
	wg.Add(N)
	results := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, err := a.Allocate(id8(1), id8(2), reqID, t0)
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	wins, dupes := 0, 0
	for err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrDuplicateRequest):
			dupes++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, wins, "exactly one Allocate should win")
	assert.Equal(t, N-1, dupes, "the rest should see ErrDuplicateRequest")
}

// TestConcurrent_SweepWithChurn runs Sweep on a tight ticker against
// goroutines that allocate/release. Race detector clean; no panics.
func TestConcurrent_SweepWithChurn(t *testing.T) {
	t0 := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	cfg := AllocatorConfig{
		MaxKbpsPerSeeder: 1024,
		SessionTTL:       30 * time.Second,
		Now:              func() time.Time { return t0 },
		Rand:             rand.Reader,
	}
	a, err := NewAllocator(cfg)
	require.NoError(t, err)

	stop := make(chan struct{})

	// Sweeper.
	var sweepWG sync.WaitGroup
	sweepWG.Add(1)
	go func() {
		defer sweepWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = a.Sweep(t0.Add(time.Hour)) // ages everything; deletes whatever's there
			}
		}
	}()

	// Workers.
	var wg sync.WaitGroup
	const N, K = 8, 64
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < K; j++ {
				var reqID [16]byte
				binary.BigEndian.PutUint64(reqID[:], uint64(i*K+j))
				s, err := a.Allocate(id8(byte(i+1)), id8(byte(i+1)), reqID, t0)
				if err != nil {
					t.Errorf("Allocate: %v", err)
					return
				}
				_, _ = a.ResolveAndCharge(s.Token, 1, t0) // may race-with-Sweep; ok
				a.Release(s.SessionID)
			}
		}()
	}
	wg.Wait()
	close(stop)
	sweepWG.Wait()
}

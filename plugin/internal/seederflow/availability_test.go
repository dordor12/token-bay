package seederflow_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/seederflow"
)

func TestAvailable_AlwaysOnDefault(t *testing.T) {
	cfg := validConfig(t)
	c, err := seederflow.New(cfg)
	require.NoError(t, err)
	require.True(t, c.Available(time.Now()))
}

func TestAvailable_RecentRateLimitWithinHeadroom(t *testing.T) {
	cfg := validConfig(t)
	cfg.HeadroomWindow = 15 * time.Minute
	c, err := seederflow.New(cfg)
	require.NoError(t, err)
	now := time.Now()
	c.RecordRateLimit(now.Add(-5 * time.Minute))
	require.False(t, c.Available(now), "should be unavailable while rate-limit signal is fresh")
	require.True(t, c.Available(now.Add(20*time.Minute)), "should be available again after headroom expires")
}

func TestAvailable_ActiveSessionBlocks(t *testing.T) {
	cfg := validConfig(t)
	cfg.ActivityGrace = 10 * time.Minute
	c, err := seederflow.New(cfg)
	require.NoError(t, err)
	now := time.Now()
	require.NoError(t, c.OnSessionStart(context.Background(), now))
	require.False(t, c.Available(now))
}

func TestAvailable_AfterSessionEndsThenGraceElapses(t *testing.T) {
	cfg := validConfig(t)
	cfg.ActivityGrace = 10 * time.Minute
	c, err := seederflow.New(cfg)
	require.NoError(t, err)
	t0 := time.Now()
	require.NoError(t, c.OnSessionStart(context.Background(), t0))
	require.NoError(t, c.OnSessionEnd(context.Background(), t0.Add(2*time.Minute)))
	// Still inside grace window.
	require.False(t, c.Available(t0.Add(5*time.Minute)))
	// Past grace window — available again.
	require.True(t, c.Available(t0.Add(20*time.Minute)))
}

func TestAvailable_ScheduledWindowInside(t *testing.T) {
	cfg := validConfig(t)
	cfg.IdlePolicy = seederflow.IdlePolicy{
		Mode:        seederflow.IdleScheduled,
		WindowStart: timeOfDay(2, 0),
		WindowEnd:   timeOfDay(6, 0),
	}
	c, err := seederflow.New(cfg)
	require.NoError(t, err)
	now := atLocalClock(time.Now(), 4, 0)
	require.True(t, c.Available(now))
}

func TestAvailable_ScheduledWindowOutside(t *testing.T) {
	cfg := validConfig(t)
	cfg.IdlePolicy = seederflow.IdlePolicy{
		Mode:        seederflow.IdleScheduled,
		WindowStart: timeOfDay(2, 0),
		WindowEnd:   timeOfDay(6, 0),
	}
	c, err := seederflow.New(cfg)
	require.NoError(t, err)
	now := atLocalClock(time.Now(), 10, 0)
	require.False(t, c.Available(now))
}

func TestAvailable_ScheduledWindowWrapAround(t *testing.T) {
	cfg := validConfig(t)
	cfg.IdlePolicy = seederflow.IdlePolicy{
		Mode:        seederflow.IdleScheduled,
		WindowStart: timeOfDay(22, 0),
		WindowEnd:   timeOfDay(6, 0),
	}
	c, err := seederflow.New(cfg)
	require.NoError(t, err)

	// Inside the late-night side.
	require.True(t, c.Available(atLocalClock(time.Now(), 23, 0)))
	// Inside the early-morning side.
	require.True(t, c.Available(atLocalClock(time.Now(), 5, 0)))
	// Outside.
	require.False(t, c.Available(atLocalClock(time.Now(), 12, 0)))
}

func timeOfDay(h, m int) time.Time {
	return time.Date(0, 1, 1, h, m, 0, 0, time.UTC)
}

func atLocalClock(d time.Time, h, m int) time.Time {
	return time.Date(d.Year(), d.Month(), d.Day(), h, m, 0, 0, d.Location())
}

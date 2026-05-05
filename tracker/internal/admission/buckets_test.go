package admission

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDayBucketIndex_StableForSameDay(t *testing.T) {
	morning := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	evening := time.Date(2026, 4, 25, 23, 59, 59, 0, time.UTC)
	assert.Equal(t, dayBucketIndex(morning, 30), dayBucketIndex(evening, 30))
}

func TestDayBucketIndex_DiffersAcrossDays(t *testing.T) {
	d1 := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
	assert.NotEqual(t, dayBucketIndex(d1, 30), dayBucketIndex(d2, 30))
}

func TestDayBucketIndex_WrapsAroundWindow(t *testing.T) {
	base := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	wrapped := base.AddDate(0, 0, 30) // exactly one window later
	assert.Equal(t, dayBucketIndex(base, 30), dayBucketIndex(wrapped, 30))
}

func TestDayBucketIndex_AlwaysInRange(t *testing.T) {
	// Spot-check 100 dates across multiple years.
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 100; i++ {
		ts := start.AddDate(0, 0, i*7) // every 7 days
		idx := dayBucketIndex(ts, 30)
		require.GreaterOrEqual(t, idx, 0)
		require.Less(t, idx, 30)
	}
}

func TestDayBucketRotateIfStale_ZeroesStale(t *testing.T) {
	b := DayBucket{Total: 5, A: 3, B: 2, DayStamp: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)}
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC) // > 30 days later
	rotated := rotateIfStale(b, now, 30)
	assert.Equal(t, uint32(0), rotated.Total)
	assert.Equal(t, uint32(0), rotated.A)
	assert.Equal(t, uint32(0), rotated.B)
	assert.True(t, rotated.DayStamp.Equal(stripToDay(now)))
}

func TestDayBucketRotateIfStale_KeepsFresh(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	b := DayBucket{Total: 5, A: 3, B: 2, DayStamp: now.AddDate(0, 0, -3)} // 3 days old
	rotated := rotateIfStale(b, now, 30)
	assert.Equal(t, uint32(5), rotated.Total)
	assert.Equal(t, uint32(3), rotated.A)
	assert.Equal(t, uint32(2), rotated.B)
}

func TestDayBucketRotateIfStale_ZeroDayStampInitializes(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	b := DayBucket{} // zero-value, never used
	rotated := rotateIfStale(b, now, 30)
	assert.Equal(t, uint32(0), rotated.Total)
	assert.True(t, rotated.DayStamp.Equal(stripToDay(now)))
}

func TestStripToDay_DropsHoursMinutesSeconds(t *testing.T) {
	ts := time.Date(2026, 4, 25, 17, 23, 45, 123456789, time.UTC)
	day := stripToDay(ts)
	assert.Equal(t, time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC), day)
}

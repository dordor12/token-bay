package admission

import "time"

// DayBucket is one day-cell in a rolling 30-day per-consumer window. The
// fields A and B are role-overloaded so a single struct serves the three
// rolling sets: settlements (Total = settled count, A = clean count, B unused),
// disputes (Total unused, A = filed count, B = upheld count), and
// flow (Total unused, A = earned credits, B = spent credits).
//
// Day-bucket arrays are time-bucketed (RollingWindowDays-length), not
// time-sorted: the bucket at index i holds events from day-of-year mod N == i.
// On a new event, callers first check DayStamp and zero the bucket if it
// belongs to a day older than the window — see rotateIfStale.
type DayBucket struct {
	Total    uint32
	A        uint32
	B        uint32
	DayStamp time.Time // start-of-day (UTC) for which this bucket is valid
}

// dayBucketIndex maps a timestamp to a bucket index in [0, windowDays).
// The unix-day count modulo windowDays scatters events across the array.
func dayBucketIndex(t time.Time, windowDays int) int {
	day := t.UTC().Unix() / 86400
	idx := int(day % int64(windowDays))
	if idx < 0 {
		idx += windowDays
	}
	return idx
}

// stripToDay returns the start-of-day (UTC) for t. Used as the canonical
// DayStamp value so two events on the same day produce identical stamps.
func stripToDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// rotateIfStale returns a copy of b with counts zeroed and DayStamp updated
// to today if the existing DayStamp is older than windowDays. Otherwise
// returns b unchanged. A zero-value DayStamp (i.e. never used) is treated
// as stale, which initializes the bucket on first use.
func rotateIfStale(b DayBucket, now time.Time, windowDays int) DayBucket {
	today := stripToDay(now)
	if b.DayStamp.IsZero() {
		return DayBucket{DayStamp: today}
	}
	age := today.Sub(b.DayStamp)
	if age >= time.Duration(windowDays)*24*time.Hour {
		return DayBucket{DayStamp: today}
	}
	// Same-window: if the DayStamp is older but within the window, the bucket
	// represents a different day-of-window than today's index expects only if
	// callers passed an inconsistent (idx, time) pair — which they don't, see
	// state.go callers. So we leave the bucket alone.
	return b
}

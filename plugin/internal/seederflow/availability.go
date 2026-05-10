package seederflow

import (
	"context"
	"time"
)

// OnSessionStart records the start of a Claude Code session at t. While
// a session is active and for ActivityGrace afterward, the coordinator
// reports unavailable so the seeder doesn't compete with the user's
// own foreground work (plugin spec §6.3, §6.4).
func (c *Coordinator) OnSessionStart(_ context.Context, t time.Time) error {
	c.mu.Lock()
	c.activityActive = true
	c.lastActivityAt = t
	c.mu.Unlock()
	return nil
}

// OnSessionEnd records the end of a Claude Code session at t. Triggers
// the activity grace timer (Available stays false until t+ActivityGrace).
func (c *Coordinator) OnSessionEnd(_ context.Context, t time.Time) error {
	c.mu.Lock()
	c.activityActive = false
	c.lastActivityAt = t
	c.mu.Unlock()
	return nil
}

// RecordRateLimit notes that the seeder's local Claude Code reported a
// rate-limit error at t. Suppresses Available for HeadroomWindow.
func (c *Coordinator) RecordRateLimit(t time.Time) {
	c.mu.Lock()
	if t.After(c.lastRateLimitAt) {
		c.lastRateLimitAt = t
	}
	c.mu.Unlock()
}

// Available reports whether the seeder should advertise available=true
// at evaluation time now. A "true" result means: idle window is open,
// activity grace has elapsed, no rate-limit signal in HeadroomWindow,
// and the bridge conformance gate hasn't tripped.
func (c *Coordinator) Available(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.availableLocked(now)
}

func (c *Coordinator) availableLocked(now time.Time) bool {
	if c.conformanceFailed {
		return false
	}
	if c.activityActive {
		return false
	}
	if !c.lastActivityAt.IsZero() && now.Sub(c.lastActivityAt) < c.cfg.ActivityGrace {
		return false
	}
	if !c.lastRateLimitAt.IsZero() && now.Sub(c.lastRateLimitAt) < c.cfg.HeadroomWindow {
		return false
	}
	if !inIdleWindow(c.cfg.IdlePolicy, now) {
		return false
	}
	return true
}

// inIdleWindow returns true when the seeder's idle policy permits
// advertising at now. always_on always returns true; scheduled checks
// the local clock against the [WindowStart, WindowEnd) interval, with
// wrap-around supported when WindowStart > WindowEnd.
func inIdleWindow(p IdlePolicy, now time.Time) bool {
	if p.Mode == IdleAlwaysOn {
		return true
	}
	curMin := now.Hour()*60 + now.Minute()
	startMin := p.WindowStart.Hour()*60 + p.WindowStart.Minute()
	endMin := p.WindowEnd.Hour()*60 + p.WindowEnd.Minute()
	if startMin == endMin {
		return false // empty window
	}
	if startMin < endMin {
		return curMin >= startMin && curMin < endMin
	}
	// Wrap-around: e.g. 22:00 → 06:00.
	return curMin >= startMin || curMin < endMin
}

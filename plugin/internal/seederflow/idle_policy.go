package seederflow

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseIdlePolicy converts a config-shaped {mode, window} pair into an
// IdlePolicy. mode must be "always_on" or "scheduled". For scheduled
// mode, window must look like "HH:MM-HH:MM" (24-hour). For always_on
// mode, window is ignored.
func ParseIdlePolicy(mode, window string) (IdlePolicy, error) {
	switch mode {
	case "always_on":
		return IdlePolicy{Mode: IdleAlwaysOn}, nil
	case "scheduled":
		start, end, err := parseWindow(window)
		if err != nil {
			return IdlePolicy{}, err
		}
		return IdlePolicy{Mode: IdleScheduled, WindowStart: start, WindowEnd: end}, nil
	default:
		return IdlePolicy{}, fmt.Errorf("seederflow: unknown idle policy mode %q", mode)
	}
}

// parseWindow returns the start and end time-of-day from a string
// like "HH:MM-HH:MM" (24-hour). The returned times have only their
// Hour/Minute fields populated; year/month/day are placeholders.
func parseWindow(s string) (time.Time, time.Time, error) {
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return time.Time{}, time.Time{}, fmt.Errorf("seederflow: idle window must be HH:MM-HH:MM, got %q", s)
	}
	start, err := parseHHMM(parts[0])
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("seederflow: idle window start: %w", err)
	}
	end, err := parseHHMM(parts[1])
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("seederflow: idle window end: %w", err)
	}
	return start, end, nil
}

func parseHHMM(s string) (time.Time, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf("expected HH:MM, got %q", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return time.Time{}, fmt.Errorf("invalid hour %q", parts[0])
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return time.Time{}, fmt.Errorf("invalid minute %q", parts[1])
	}
	return time.Date(0, 1, 1, h, m, 0, 0, time.UTC), nil
}

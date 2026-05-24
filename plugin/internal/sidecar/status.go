package sidecar

import (
	"time"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
)

// Status is a snapshot of supervisor + subsystem state, suitable for
// rendering by `/token-bay status`.
type Status struct {
	Running    bool
	StartedAt  time.Time
	CCProxyURL string
	Tracker    trackerclient.ConnectionState
}

// Status returns a current snapshot. Safe to call concurrently with Run.
func (a *App) Status() Status {
	a.startMu.Lock()
	running := a.started && !a.closed
	startedAt := a.startedAt
	a.startMu.Unlock()

	return Status{
		Running:    running,
		StartedAt:  startedAt,
		CCProxyURL: a.proxy.URL(),
		Tracker:    a.tracker.Status(),
	}
}

// statusSnapshot is the closure handed to ccproxy.SetStatusProvider —
// renders Status() as a CLI-friendly map. Tracker.State stringifies via
// its String() method (trackerclient.ConnectionState is a Stringer).
func (a *App) statusSnapshot() any {
	s := a.Status()
	return map[string]any{
		"running":     s.Running,
		"started_at":  s.StartedAt.UTC().Format(time.RFC3339),
		"ccproxy_url": s.CCProxyURL,
		"tracker":     s.Tracker.Phase.String(),
	}
}

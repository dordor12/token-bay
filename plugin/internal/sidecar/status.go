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

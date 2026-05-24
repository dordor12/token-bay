package federation

import (
	"context"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// healthWatcherState tracks how long a peer has been below the
// low-health threshold. Once it sustains below for the configured
// window, the watcher fires Federation.Depeer.
type healthWatcherState struct {
	belowSince map[ids.TrackerID]time.Time
}

// runHealthWatcher (slice 11) periodically scores every active peer
// and depeers any whose score stays below LowHealthThreshold for at
// least LowHealthSustainedWindow. Zero threshold or zero window
// disables. Allowlist + already-depeered peers are skipped.
//
// Hysteresis: a peer that briefly dips and recovers is forgiven —
// belowSince resets when the score rises back above the threshold.
func (f *Federation) runHealthWatcher(ctx context.Context) {
	if f.cfg.LowHealthThreshold <= 0 || f.cfg.LowHealthSustainedWindow <= 0 {
		return
	}
	interval := f.cfg.HealthWatchInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	state := healthWatcherState{belowSince: map[ids.TrackerID]time.Time{}}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			f.runHealthWatcherTick(now, &state)
		}
	}
}

// runHealthWatcherTick is the single-iteration body. Exposed for tests.
func (f *Federation) runHealthWatcherTick(now time.Time, state *healthWatcherState) {
	for _, p := range f.reg.All() {
		if p.State != PeerStateSteady {
			delete(state.belowSince, p.TrackerID)
			continue
		}
		score := f.health.Score(p.TrackerID, now)
		if score < f.cfg.LowHealthThreshold {
			since, ok := state.belowSince[p.TrackerID]
			if !ok {
				state.belowSince[p.TrackerID] = now
				continue
			}
			if now.Sub(since) >= f.cfg.LowHealthSustainedWindow {
				f.dep.Logger.Warn().
					Hex("peer", p.TrackerID[:]).
					Float64("score", score).
					Msg("federation: depeering on sustained low health score")
				_ = f.Depeer(p.TrackerID, ReasonLowHealth)
				delete(state.belowSince, p.TrackerID)
			}
		} else {
			delete(state.belowSince, p.TrackerID)
		}
	}
}

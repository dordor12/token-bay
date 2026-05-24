package federation

import (
	"context"
	"time"
)

// KnownPeersPruner is the optional storage hook for slice-10
// auto-pruning. Only *storage.Store satisfies it; tests can inject a
// fake. nil disables pruning entirely.
type KnownPeersPruner interface {
	DeleteStaleKnownPeers(ctx context.Context, cutoff time.Time) (int64, error)
}

// runKnownPeersPruner is the slice-10 periodic pruner. Wakes every
// f.cfg.KnownPeersPruneInterval and removes gossip-sourced rows whose
// last_seen is older than (now - f.cfg.KnownPeersMaxAge). Allowlist
// rows are immune by SQL filter.
//
// Cancelled via the existing listenCtx. Errors are logged at Warn.
func (f *Federation) runKnownPeersPruner(ctx context.Context) {
	pr, ok := f.dep.KnownPeers.(KnownPeersPruner)
	if !ok {
		return
	}
	if f.cfg.KnownPeersPruneInterval <= 0 || f.cfg.KnownPeersMaxAge <= 0 {
		return
	}
	t := time.NewTicker(f.cfg.KnownPeersPruneInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cutoff := f.dep.Now().Add(-f.cfg.KnownPeersMaxAge)
			n, err := pr.DeleteStaleKnownPeers(ctx, cutoff)
			if err != nil {
				f.dep.Logger.Warn().Err(err).Msg("federation: known_peers pruner failed")
				continue
			}
			if n > 0 {
				f.dep.Logger.Info().Int64("removed", n).Msg("federation: known_peers pruner removed stale gossip rows")
			}
		}
	}
}

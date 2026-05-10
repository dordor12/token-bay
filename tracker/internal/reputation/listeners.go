package reputation

import (
	"context"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// FreezeListener is the reputation→outside notification hook. The
// evaluator calls OnFreeze synchronously when an identity transitions
// to FROZEN. Implementations MUST NOT block — federation's impl signs
// and enqueues the gossip via per-peer send queues, returning before
// the network round-trip completes.
//
// federation.*Federation satisfies this interface via Go structural
// typing; no compile-time assertion or import is needed (per the
// reputation-leaf-module rule in CLAUDE.md).
type FreezeListener interface {
	OnFreeze(ctx context.Context, id ids.IdentityID, reason string, revokedAt time.Time)
}

// WithFreezeListener registers a FreezeListener that the evaluator
// calls on every AUDIT->FROZEN transition. Optional; nil disables
// the hook.
func WithFreezeListener(l FreezeListener) Option {
	return func(s *Subsystem) { s.freezeListener = l }
}

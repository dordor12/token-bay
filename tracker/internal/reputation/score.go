package reputation

import (
	"context"

	"github.com/token-bay/token-bay/shared/ids"
)

// Score returns the cached reputation score for id. ok=false on cache
// miss; callers (the broker) treat that as "no opinion" and fall back
// to defaults.
//
// Never blocks on storage. Returns cfg.DefaultScore on miss.
func (s *Subsystem) Score(id ids.IdentityID) (float64, bool) {
	cache := s.cache.Load()
	if cache == nil {
		return s.cfg.DefaultScore, false
	}
	e, ok := cache.states[idsKey(id)]
	if !ok {
		return s.cfg.DefaultScore, false
	}
	return e.Score, true
}

// IsFrozen reports whether id is currently in the FROZEN state. Returns
// false on cache miss. Never blocks on storage.
func (s *Subsystem) IsFrozen(id ids.IdentityID) bool {
	cache := s.cache.Load()
	if cache == nil {
		return false
	}
	e, ok := cache.states[idsKey(id)]
	if !ok {
		return false
	}
	return e.Frozen
}

// Status returns the full reputation status for id. The Reasons slice
// is fetched from rep_state on demand (one DB round-trip) since it can
// be long. Cache miss returns a zero-valued ReputationStatus with
// state=OK.
//
// Used by the (deferred) admin API. Not on the broker hot path.
func (s *Subsystem) Status(id ids.IdentityID) ReputationStatus {
	cache := s.cache.Load()
	if cache == nil {
		return ReputationStatus{}
	}
	e, ok := cache.states[idsKey(id)]
	if !ok {
		return ReputationStatus{}
	}
	out := ReputationStatus{State: e.State, Since: e.Since}
	row, found, err := s.store.readState(context.Background(), id)
	if err == nil && found {
		out.Reasons = row.Reasons
	}
	return out
}

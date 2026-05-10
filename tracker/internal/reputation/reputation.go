package reputation

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/config"
)

// Subsystem is the process-wide reputation value. Construct via Open;
// tear down via Close.
type Subsystem struct { //nolint:revive // package name is reputation; Subsystem is the canonical handle
	cfg   config.ReputationConfig
	nowFn func() time.Time
	store *storage

	// breachMu guards categorical-breach transitions. The evaluator
	// does not take this — it serializes itself by virtue of being a
	// single goroutine.
	breachMu sync.Mutex

	// cache is atomically swapped by the evaluator each cycle and
	// snapshot-loaded on Open. Hot-path reads (Score / IsFrozen /
	// Status) do one Load and one map lookup.
	cache atomic.Pointer[scoreCache]

	metrics *metrics

	freezeListener FreezeListener

	closeMu sync.Mutex
	closed  atomic.Bool
	stop    chan struct{}
	wg      sync.WaitGroup
}

// Option configures optional Subsystem dependencies on Open.
type Option func(*Subsystem)

// WithClock overrides time.Now for testing.
func WithClock(now func() time.Time) Option {
	return func(s *Subsystem) { s.nowFn = now }
}

// scoreCache is the read-side snapshot. The map is never mutated after
// publish; callers only ever Load() the pointer.
type scoreCache struct {
	states map[idsKey]cachedEntry
}

type idsKey [32]byte

type cachedEntry struct {
	State  State
	Score  float64
	Since  time.Time
	Frozen bool
}

// Open constructs a Subsystem, opens its SQLite DB, applies the schema,
// seeds the read cache from any existing rep_state / rep_scores rows,
// and starts the evaluator goroutine.
func Open(ctx context.Context, cfg config.ReputationConfig, opts ...Option) (*Subsystem, error) {
	if cfg.StoragePath == "" {
		return nil, errors.New("reputation: Open requires cfg.StoragePath")
	}
	if cfg.EvaluationIntervalS <= 0 {
		return nil, fmt.Errorf("reputation: EvaluationIntervalS = %d must be > 0",
			cfg.EvaluationIntervalS)
	}
	if cfg.MinPopulationForZScore <= 0 {
		return nil, fmt.Errorf("reputation: MinPopulationForZScore = %d must be > 0",
			cfg.MinPopulationForZScore)
	}

	store, err := openStorage(ctx, cfg.StoragePath)
	if err != nil {
		return nil, err
	}

	s := &Subsystem{
		cfg:   cfg,
		nowFn: time.Now,
		store: store,
		stop:  make(chan struct{}),
	}
	for _, o := range opts {
		o(s)
	}

	s.metrics = newMetrics()

	if err := s.reloadCache(ctx); err != nil {
		_ = store.Close()
		return nil, err
	}

	s.startEvaluator()
	return s, nil
}

// Close stops the evaluator goroutine and closes the DB. Idempotent.
func (s *Subsystem) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed.Load() {
		return nil
	}
	s.closed.Store(true)
	close(s.stop)
	s.wg.Wait()
	return s.store.Close()
}

// Register exposes the package's Prometheus collectors. Call once,
// typically from the tracker's metrics bootstrap.
func (s *Subsystem) Register(r prometheus.Registerer) error {
	return s.metrics.register(r)
}

// reloadCache loads every rep_state and rep_scores row and atomically
// publishes a fresh scoreCache. Called from Open and from each evaluator
// cycle.
func (s *Subsystem) reloadCache(ctx context.Context) error {
	states, err := s.store.loadAllStates(ctx)
	if err != nil {
		return err
	}
	scores, err := s.store.loadAllScores(ctx)
	if err != nil {
		return err
	}
	cache := &scoreCache{states: make(map[idsKey]cachedEntry, len(states))}
	for id, st := range states {
		sc, ok := scores[id]
		if !ok {
			// No rep_scores row yet (e.g. first ingest before first
			// evaluator cycle). Falling through to DefaultScore avoids
			// returning a 0.0 score to the broker for an identity that
			// has only just been registered.
			sc = s.cfg.DefaultScore
		}
		cache.states[idsKey(id)] = cachedEntry{
			State:  st.State,
			Score:  sc,
			Since:  st.Since,
			Frozen: st.State == StateFrozen,
		}
	}
	for id, sc := range scores {
		if _, ok := cache.states[idsKey(id)]; !ok {
			cache.states[idsKey(id)] = cachedEntry{
				State: StateOK, Score: sc,
			}
		}
	}
	s.cache.Store(cache)
	return nil
}

// now returns the configured wall clock.
func (s *Subsystem) now() time.Time { return s.nowFn() }

// idKey converts an ids.IdentityID to the map key type.
func idKey(id ids.IdentityID) idsKey { return idsKey(id) } //nolint:unused // wired in Task 16

// startEvaluator launches the periodic goroutine. Real loop lands in
// Task 16; until then, runEvaluator just blocks on stop.
func (s *Subsystem) startEvaluator() {
	s.wg.Add(1)
	go s.runEvaluator()
}

func (s *Subsystem) runEvaluator() {
	defer s.wg.Done()
	interval := time.Duration(s.cfg.EvaluationIntervalS) * time.Second
	t := time.NewTimer(interval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), interval)
			if err := s.runOneCycleSafe(ctx); err != nil {
				// Log path lands in Task 17 — for now, eat the error
				// (drop-event-not-process is the spec §11 behavior).
				_ = err
			}
			cancel()
			t.Reset(interval)
		}
	}
}

// notifyFreeze invokes the configured FreezeListener with panic
// recovery so a misbehaving listener cannot crash the evaluator
// goroutine. No-op when no listener is configured.
func (s *Subsystem) notifyFreeze(ctx context.Context, id ids.IdentityID, reason string, revokedAt time.Time) {
	if s.freezeListener == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.metrics.evaluatorPanics.Inc()
		}
	}()
	s.freezeListener.OnFreeze(ctx, id, reason, revokedAt)
}

// runOneCycleSafe wraps runOneCycle with panic recovery so a single
// evaluator panic doesn't kill the goroutine.
func (s *Subsystem) runOneCycleSafe(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			s.metrics.evaluatorPanics.Inc()
			err = fmt.Errorf("reputation: evaluator panic: %v", r)
		}
	}()
	return s.runOneCycle(ctx)
}

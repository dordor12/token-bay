package ledger

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

// Ledger is the chain orchestrator. One per tracker process.
//
// The orchestrator does not own its Store — the caller is responsible for
// closing it independently. This separation lets tests reuse a single
// Store across multiple Ledger configurations and makes the lifecycle
// boundaries explicit.
type Ledger struct {
	store      *storage.Store
	trackerKey ed25519.PrivateKey
	trackerPub ed25519.PublicKey
	nowFn      func() time.Time
	mu         sync.Mutex

	// stop is closed by Close to signal background goroutines (the
	// Merkle rollup loop) to drain. wg tracks them so Close can wait.
	// closeOnce guards stop against double-close.
	stop      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// Option configures optional dependencies on Open.
type Option func(*Ledger)

// WithClock overrides time.Now for testing. The function is called once
// per timestamp-using operation (entry timestamps, snapshot issued_at).
func WithClock(now func() time.Time) Option {
	return func(l *Ledger) { l.nowFn = now }
}

// Open wires the orchestrator over an open Store and a tracker keypair.
// Returns an error on a nil store or wrong-length key. The caller retains
// ownership of the Store and must close it independently.
func Open(store *storage.Store, key ed25519.PrivateKey, opts ...Option) (*Ledger, error) {
	if store == nil {
		return nil, errors.New("ledger: Open requires a non-nil Store")
	}
	if len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("ledger: tracker key length %d, want %d", len(key), ed25519.PrivateKeySize)
	}
	l := &Ledger{
		store:      store,
		trackerKey: key,
		trackerPub: key.Public().(ed25519.PublicKey),
		nowFn:      time.Now,
		stop:       make(chan struct{}),
	}
	for _, opt := range opts {
		opt(l)
	}
	return l, nil
}

// Close signals background goroutines (started by StartRollup) to
// drain and waits for them. Safe to call multiple times; subsequent
// calls are no-ops. Does NOT close the underlying Store — the caller
// retains Store ownership.
func (l *Ledger) Close() error {
	l.closeOnce.Do(func() { close(l.stop) })
	l.wg.Wait()
	return nil
}

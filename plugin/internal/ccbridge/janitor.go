package ccbridge

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ActiveClientChecker is implemented by the seeder coordinator
// (or any callsite that knows the live set of P2P peers). The
// janitor consults it to decide whether a per-client session
// folder is still in use.
//
// Implementations must be safe for concurrent reads from the
// janitor goroutine.
type ActiveClientChecker interface {
	// IsActive returns true if pub is currently connected /
	// transacting with the seeder. The semantics of "active" are
	// defined by the implementation; common choices include "has
	// an open QUIC stream" or "has issued a forwarded request in
	// the last N minutes."
	IsActive(pub ed25519.PublicKey) bool
}

// ActiveByHashChecker is an optional extension to
// ActiveClientChecker for callers (like the janitor) that only
// have the client hash on hand. Implementing it lets the janitor
// skip the pubkey reverse lookup.
type ActiveByHashChecker interface {
	IsActiveByHash(hash string) bool
}

// Janitor reaps per-client session folders under Root once their
// owning client is no longer active and the folder hasn't been
// touched within Grace.
//
// Wiring example for the seeder daemon (cmd/token-bay-sidecar):
//
//	import "github.com/token-bay/token-bay/plugin/internal/ccbridge"
//
//	// coord implements both ccbridge.ActiveClientChecker and
//	// ccbridge.ActiveByHashChecker by consulting its peer map.
//	j := &ccbridge.Janitor{
//		Root:     filepath.Join(cfg.DataDir, "seeder-sessions"),
//		Checker:  coord,
//		Grace:    30 * time.Minute,
//		Interval: 5 * time.Minute,
//	}
//	go j.Run(ctx) // ctx cancelled on daemon shutdown
//
// Set ExecRunner.SeederRoot to the same Root so the runner writes
// session files where the janitor will scan.
type Janitor struct {
	Root     string
	Checker  ActiveClientChecker
	Grace    time.Duration
	Interval time.Duration
}

// Scan performs a single sweep of Root. Returns the slice of
// client-hash directory names that were removed (for logging).
func (j *Janitor) Scan() ([]string, error) {
	if j.Root == "" {
		return nil, fmt.Errorf("ccbridge: janitor Root is empty")
	}
	if j.Checker == nil {
		return nil, fmt.Errorf("ccbridge: janitor Checker is nil")
	}
	entries, err := os.ReadDir(j.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("ccbridge: read seeder root: %w", err)
	}
	var removed []string
	cutoff := time.Now().Add(-j.Grace)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !looksLikeClientHash(name) {
			continue
		}
		fullPath := filepath.Join(j.Root, name)
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue // within grace window
		}
		if isActiveByHash(j.Checker, name) {
			continue
		}
		if err := os.RemoveAll(fullPath); err != nil {
			// Logged-not-fatal: a stuck folder shouldn't kill the
			// janitor. Implementations can surface this via
			// metrics if they care.
			continue
		}
		removed = append(removed, name)
	}
	return removed, nil
}

// Run starts a periodic loop that calls Scan every Interval.
// Returns when ctx is cancelled. Safe to launch in its own
// goroutine; not safe to invoke concurrently on the same
// Janitor instance.
//
// First scan fires immediately so callers don't wait Interval
// before the first sweep.
func (j *Janitor) Run(ctx context.Context) {
	if j.Interval <= 0 {
		j.Interval = time.Minute
	}
	t := time.NewTicker(j.Interval)
	defer t.Stop()

	_, _ = j.Scan()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = j.Scan()
		}
	}
}

// looksLikeClientHash returns true iff name is exactly 16 hex chars
// (the format ClientHash produces).
func looksLikeClientHash(name string) bool {
	if len(name) != 16 {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil
}

// isActiveByHash bridges between the interface (which takes a
// PublicKey) and the janitor's view of disk (which only has
// hashes). Implementations of ActiveByHashChecker get a precise
// reverse lookup; otherwise we conservatively report "active"
// (keep the folder) since we can't reconstruct the pubkey from
// the hash alone.
func isActiveByHash(c ActiveClientChecker, hash string) bool {
	if hc, ok := c.(ActiveByHashChecker); ok {
		return hc.IsActiveByHash(hash)
	}
	return true
}

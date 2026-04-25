package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"
)

// Store is the typed durable layer over a SQLite ledger DB. One Store per
// tracker process; the orchestrator owns its lifecycle.
type Store struct {
	db      *sql.DB
	writeMu sync.Mutex // serializes append-class operations; reads do not take this
	closeMu sync.Mutex
	closed  bool
}

// Open opens (or creates) the ledger DB at path, applies migrations, and
// returns a ready Store. Caller is responsible for calling Close.
//
// Pragmas configured by the DSN:
//   - journal_mode=WAL — durable + read-while-write concurrency
//   - synchronous=NORMAL — documented WAL pairing (FULL is overkill)
//   - busy_timeout=5000 — wait 5s on transient lock contention
//   - foreign_keys=ON — defensive; v1 schema has no FKs but cheap to enable
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("storage: Open requires a non-empty path")
	}
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: sql.Open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: ping: %w", err)
	}
	if err := applyMigrations(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close releases the underlying SQLite handle. Safe to call multiple times.
func (s *Store) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}

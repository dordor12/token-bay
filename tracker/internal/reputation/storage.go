package reputation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"
)

// storage is the typed durable layer over a SQLite reputation DB. One
// storage per Subsystem; the Subsystem owns its lifecycle.
type storage struct {
	db      *sql.DB
	writeMu sync.Mutex //nolint:unused // used by future write methods (Task 5+)
	closeMu sync.Mutex
	closed  bool
}

// openStorage opens (or creates) the reputation DB at path, applies the
// v1 schema, and returns a ready handle. Pragmas mirror
// tracker/internal/ledger/storage/store.go.
func openStorage(ctx context.Context, path string) (*storage, error) {
	if path == "" {
		return nil, errors.New("reputation: storage requires a non-empty path")
	}
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("reputation: sql.Open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("reputation: ping: %w", err)
	}
	if err := applySchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &storage{db: db}, nil
}

// Close releases the underlying SQLite handle. Idempotent.
func (s *storage) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}

// sqlNoRows is a small indirection so other storage_*.go files can
// reference sql.ErrNoRows without each importing database/sql for that
// single sentinel.
func sqlNoRows() error { return sql.ErrNoRows } //nolint:unused // consumed by future storage_*.go files (Task 7+)

func applySchema(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS rep_state (
            identity_id    BLOB    NOT NULL PRIMARY KEY CHECK(length(identity_id) = 32),
            state          INTEGER NOT NULL,
            since          INTEGER NOT NULL,
            first_seen_at  INTEGER NOT NULL,
            reasons        TEXT    NOT NULL DEFAULT '[]',
            updated_at     INTEGER NOT NULL
         )`,
		`CREATE TABLE IF NOT EXISTS rep_events (
            id           INTEGER PRIMARY KEY AUTOINCREMENT,
            identity_id  BLOB    NOT NULL CHECK(length(identity_id) = 32),
            role         INTEGER NOT NULL,
            event_type   INTEGER NOT NULL,
            value        REAL    NOT NULL,
            observed_at  INTEGER NOT NULL
         )`,
		`CREATE INDEX IF NOT EXISTS idx_rep_events_id_time   ON rep_events(identity_id, observed_at)`,
		`CREATE INDEX IF NOT EXISTS idx_rep_events_type_time ON rep_events(event_type,  observed_at)`,
		`CREATE TABLE IF NOT EXISTS rep_scores (
            identity_id  BLOB    NOT NULL PRIMARY KEY CHECK(length(identity_id) = 32),
            score        REAL    NOT NULL,
            updated_at   INTEGER NOT NULL
         )`,
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("reputation: schema: %w", err)
		}
	}
	return nil
}

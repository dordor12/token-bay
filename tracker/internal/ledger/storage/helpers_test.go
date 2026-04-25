package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// openTempDB returns a SQLite handle backed by a fresh tempdir-scoped file
// with v1 migrations applied. Closes automatically via t.Cleanup.
//
// Used by tests that need raw SQL access without the Store wrapper.
// Higher-level tests use openTempStore.
func openTempDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "ledger.db") +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, applyMigrations(context.Background(), db))
	return db
}

// openTempStore returns a fresh Store backed by a tempdir-scoped DB file.
// Closes automatically via t.Cleanup.
func openTempStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.db")
	s, err := Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

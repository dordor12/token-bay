package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestApplyMigrations_CreatesV1Tables(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "ledger.db")+"?_pragma=journal_mode(WAL)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	require.NoError(t, applyMigrations(ctx, db))

	for _, name := range []string{"schema_version", "entries", "balances", "merkle_roots", "peer_root_archive"} {
		var got string
		err := db.QueryRowContext(ctx,
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&got)
		require.NoError(t, err, "table %q missing", name)
		assert.Equal(t, name, got)
	}

	var version int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT version FROM schema_version").Scan(&version))
	assert.Equal(t, 1, version)
}

func TestApplyMigrations_Idempotent(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "ledger.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	require.NoError(t, applyMigrations(ctx, db))
	require.NoError(t, applyMigrations(ctx, db), "running twice must not error")

	var n int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_version").Scan(&n))
	assert.Equal(t, 1, n, "schema_version row should exist exactly once")
}

func TestApplyMigrations_CreatesIndexes(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "ledger.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	require.NoError(t, applyMigrations(context.Background(), db))

	for _, name := range []string{"idx_entries_consumer", "idx_entries_seeder", "idx_entries_time"} {
		var got string
		err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='index' AND name=?", name).Scan(&got)
		require.NoError(t, err, "index %q missing", name)
		assert.Equal(t, name, got)
	}
}

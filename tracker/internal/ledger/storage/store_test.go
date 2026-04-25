package storage

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpen_CreatesFileAndAppliesSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	s, err := Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	row := s.db.QueryRow("SELECT version FROM schema_version")
	var v int
	require.NoError(t, row.Scan(&v))
	assert.Equal(t, 1, v)
}

func TestOpen_ReusesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	s1, err := Open(context.Background(), path)
	require.NoError(t, err)
	require.NoError(t, s1.Close())

	s2, err := Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	var v int
	require.NoError(t, s2.db.QueryRow("SELECT version FROM schema_version").Scan(&v))
	assert.Equal(t, 1, v, "reopening must not duplicate schema rows")

	var n int
	require.NoError(t, s2.db.QueryRow("SELECT COUNT(*) FROM schema_version").Scan(&n))
	assert.Equal(t, 1, n)
}

func TestOpen_RejectsEmptyPath(t *testing.T) {
	_, err := Open(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-empty path")
}

func TestClose_Idempotent(t *testing.T) {
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "ledger.db"))
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.NoError(t, s.Close(), "closing twice should not error")
}

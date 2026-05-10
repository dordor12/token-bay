package reputation

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStorageOpen_AppliesSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "rep.sqlite")
	s, err := openStorage(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	rows, err := s.db.QueryContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type IN ('table','index')
         AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	require.NoError(t, err)
	defer rows.Close()

	var got []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		got = append(got, n)
	}
	sort.Strings(got)

	want := []string{
		"idx_rep_events_id_time",
		"idx_rep_events_type_time",
		"rep_events",
		"rep_scores",
		"rep_state",
	}
	require.Equal(t, want, got)
}

func TestStorageOpen_IsIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "rep.sqlite")
	s, err := openStorage(context.Background(), dbPath)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	s2, err := openStorage(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	var n int
	require.NoError(t,
		s2.db.QueryRow(`SELECT count(*) FROM sqlite_master
                         WHERE type='table' AND name='rep_state'`).Scan(&n))
	require.Equal(t, 1, n)
}

func TestStorageOpen_RejectsEmptyPath(t *testing.T) {
	_, err := openStorage(context.Background(), "")
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "non-empty path"))
}

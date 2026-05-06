package auditlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRotate_MovesContentToArchiveAndOpensFresh(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "audit.log")
	l, err := Open(p)
	require.NoError(t, err)
	l.now = func() time.Time { return time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC) }

	require.NoError(t, l.LogConsumer(ConsumerRecord{RequestID: "before-rotate", Timestamp: time.Now().UTC()}))
	require.NoError(t, l.Rotate())
	require.NoError(t, l.LogConsumer(ConsumerRecord{RequestID: "after-rotate", Timestamp: time.Now().UTC()}))
	require.NoError(t, l.Close())

	archive := p + ".20260507T120000Z"
	_, err = os.Stat(archive)
	require.NoError(t, err, "expected archive at %s", archive)

	archLines := readLines(t, archive)
	require.Len(t, archLines, 1)
	freshLines := readLines(t, p)
	require.Len(t, freshLines, 1)

	var ar, fr map[string]any
	require.NoError(t, json.Unmarshal(archLines[0], &ar))
	require.NoError(t, json.Unmarshal(freshLines[0], &fr))
	assert.Equal(t, "before-rotate", ar["request_id"])
	assert.Equal(t, "after-rotate", fr["request_id"])
}

func TestRotate_FreshFileHasMode0600(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(p)
	require.NoError(t, err)
	defer l.Close()

	require.NoError(t, l.Rotate())

	info, err := os.Stat(p)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestRotate_AfterCloseReturnsErrClosed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(p)
	require.NoError(t, err)
	require.NoError(t, l.Close())

	err = l.Rotate()
	assert.ErrorIs(t, err, ErrClosed)
}

func TestRotate_SameSecondCollisionGetsSuffix(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "audit.log")
	l, err := Open(p)
	require.NoError(t, err)
	defer l.Close()
	frozen := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return frozen }

	require.NoError(t, l.LogConsumer(ConsumerRecord{RequestID: "a", Timestamp: frozen}))
	require.NoError(t, l.Rotate())
	require.NoError(t, l.LogConsumer(ConsumerRecord{RequestID: "b", Timestamp: frozen}))
	require.NoError(t, l.Rotate())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var archives []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "audit.log.20260507T120000Z") {
			archives = append(archives, e.Name())
		}
	}
	assert.ElementsMatch(t,
		[]string{"audit.log.20260507T120000Z", "audit.log.20260507T120000Z-1"},
		archives,
	)
}

func TestRotate_OnEmptyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(p)
	require.NoError(t, err)
	defer l.Close()
	frozen := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return frozen }

	require.NoError(t, l.Rotate())

	archive := p + ".20260507T120000Z"
	info, err := os.Stat(archive)
	require.NoError(t, err)
	assert.Equal(t, int64(0), info.Size())
	_, err = os.Stat(p)
	require.NoError(t, err, "live tail re-created")
}

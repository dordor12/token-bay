package auditlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpen_CreatesFileMode0600(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")

	l, err := Open(p)
	require.NoError(t, err)
	require.NoError(t, l.Close())

	info, err := os.Stat(p)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestOpen_ExistingFilePreservesContent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	require.NoError(t, os.WriteFile(p, []byte("seed-line\n"), 0o600))

	l, err := Open(p)
	require.NoError(t, err)
	require.NoError(t, l.LogConsumer(ConsumerRecord{
		RequestID: "after-seed",
		Timestamp: time.Now().UTC(),
	}))
	require.NoError(t, l.Close())

	raw, err := os.ReadFile(p)
	require.NoError(t, err)
	lines := bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n"))
	require.Len(t, lines, 2)
	assert.Equal(t, "seed-line", string(lines[0]))

	var probe map[string]any
	require.NoError(t, json.Unmarshal(lines[1], &probe))
	assert.Equal(t, "consumer", probe["kind"])
	assert.Equal(t, "after-seed", probe["request_id"])
}

func TestLogger_LogConsumerAndSeederAppendValidLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(p)
	require.NoError(t, err)
	defer l.Close()

	now := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	require.NoError(t, l.LogConsumer(ConsumerRecord{
		RequestID:   "c1",
		CostCredits: 7,
		Timestamp:   now,
	}))
	require.NoError(t, l.LogSeeder(SeederRecord{
		RequestID:      "s1",
		Model:          "claude-haiku-4-5",
		ConsumerIDHash: mustHash32(t, "ab"),
		StartedAt:      now,
		CompletedAt:    now.Add(time.Second),
	}))
	require.NoError(t, l.Close())

	lines := readLines(t, p)
	require.Len(t, lines, 2)

	var c, s map[string]any
	require.NoError(t, json.Unmarshal(lines[0], &c))
	require.NoError(t, json.Unmarshal(lines[1], &s))
	assert.Equal(t, "consumer", c["kind"])
	assert.Equal(t, "c1", c["request_id"])
	assert.Equal(t, "seeder", s["kind"])
	assert.Equal(t, "s1", s["request_id"])
}

func TestLogger_AfterCloseReturnsErrClosed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(p)
	require.NoError(t, err)
	require.NoError(t, l.Close())

	err = l.LogConsumer(ConsumerRecord{RequestID: "x", Timestamp: time.Now().UTC()})
	assert.ErrorIs(t, err, ErrClosed)

	err = l.LogSeeder(SeederRecord{
		RequestID:      "y",
		ConsumerIDHash: mustHash32(t, "11"),
		StartedAt:      time.Now().UTC(),
		CompletedAt:    time.Now().UTC(),
	})
	assert.ErrorIs(t, err, ErrClosed)
}

func TestLogger_CloseIsIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(p)
	require.NoError(t, err)
	require.NoError(t, l.Close())
	assert.NoError(t, l.Close())
}

func TestLogger_ConcurrentAppendsAreLineAtomic(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(p)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	const N = 200
	var wg sync.WaitGroup
	wg.Add(N)
	for i := range N {
		go func(i int) {
			defer wg.Done()
			err := l.LogConsumer(ConsumerRecord{
				RequestID:   "req-" + strconv.Itoa(i),
				CostCredits: int64(i),
				Timestamp:   time.Now().UTC(),
			})
			assert.NoError(t, err)
		}(i)
	}
	wg.Wait()
	require.NoError(t, l.Close())

	lines := readLines(t, p)
	assert.Len(t, lines, N)
	seen := make(map[string]bool, N)
	for _, line := range lines {
		var probe map[string]any
		require.NoError(t, json.Unmarshal(line, &probe), "line=%q", string(line))
		assert.Equal(t, "consumer", probe["kind"])
		seen[probe["request_id"].(string)] = true
	}
	assert.Len(t, seen, N, "every request_id should be unique and present")
}

func TestLogger_PathReturnsOpenedPath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(p)
	require.NoError(t, err)
	defer l.Close()
	assert.Equal(t, p, l.Path())
}

func readLines(t *testing.T, path string) [][]byte {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var out [][]byte
	for sc.Scan() {
		out = append(out, append([]byte(nil), sc.Bytes()...))
	}
	require.NoError(t, sc.Err())
	return out
}

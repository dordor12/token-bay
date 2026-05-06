package admission

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTLogWriter_AppendThenRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.tlog")
	w, err := newTLogWriter(path, 5*time.Millisecond, 1<<30)
	require.NoError(t, err)
	defer w.Close()

	rec := TLogRecord{Seq: 1, Ts: 100, Kind: TLogKindSettlement, Payload: []byte("hello")}
	require.NoError(t, w.Append(rec))
	require.NoError(t, w.Close())

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	parsed, _, err := unmarshalTLogRecord(got)
	require.NoError(t, err)
	assert.Equal(t, rec, parsed)
}

func TestTLogWriter_DisputeSyncImmediate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.tlog")
	w, err := newTLogWriter(path, 1*time.Hour /*long batch window*/, 1<<30)
	require.NoError(t, err)
	defer w.Close()

	rec := TLogRecord{Seq: 1, Ts: 100, Kind: TLogKindDisputeFiled, Payload: []byte("d1")}
	require.NoError(t, w.Append(rec))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotEmpty(t, got)
}

func TestTLogWriter_BatchedFlushHonorsCadence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.tlog")
	w, err := newTLogWriter(path, 50*time.Millisecond, 1<<30)
	require.NoError(t, err)
	defer w.Close()

	rec := TLogRecord{Seq: 1, Ts: 100, Kind: TLogKindSettlement, Payload: []byte("s1")}
	require.NoError(t, w.Append(rec))

	time.Sleep(150 * time.Millisecond)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotEmpty(t, got)
}

func TestTLogWriter_Rotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.tlog")

	w, err := newTLogWriter(path, 5*time.Millisecond, 200 /*bytes*/)
	require.NoError(t, err)
	defer w.Close()

	for i := uint64(1); i <= 10; i++ {
		require.NoError(t, w.Append(TLogRecord{
			Seq: i, Ts: 100, Kind: TLogKindSettlement, Payload: []byte("x12345678901234567890"),
		}))
	}
	require.NoError(t, w.Close())

	matches, err := filepath.Glob(path + ".*")
	require.NoError(t, err)
	assert.NotEmpty(t, matches, "rotated files should exist after exceeding the cap")
}

func TestTLogWriter_ConcurrentAppendsRaceClean(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.tlog")
	w, err := newTLogWriter(path, 5*time.Millisecond, 1<<30)
	require.NoError(t, err)
	defer w.Close()

	var wg sync.WaitGroup
	for i := uint64(1); i <= 100; i++ {
		wg.Add(1)
		go func(seq uint64) {
			defer wg.Done()
			err := w.Append(TLogRecord{Seq: seq, Ts: 100, Kind: TLogKindSettlement, Payload: []byte("z")})
			assert.NoError(t, err)
		}(i)
	}
	wg.Wait()
	require.NoError(t, w.Close())
}

func TestTLogWriter_OpenAppendsToExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.tlog")

	w1, err := newTLogWriter(path, 5*time.Millisecond, 1<<30)
	require.NoError(t, err)
	require.NoError(t, w1.Append(TLogRecord{Seq: 1, Ts: 100, Kind: TLogKindSettlement, Payload: []byte("a")}))
	require.NoError(t, w1.Close())

	w2, err := newTLogWriter(path, 5*time.Millisecond, 1<<30)
	require.NoError(t, err)
	require.NoError(t, w2.Append(TLogRecord{Seq: 2, Ts: 101, Kind: TLogKindSettlement, Payload: []byte("b")}))
	require.NoError(t, w2.Close())

	got, err := os.ReadFile(path)
	require.NoError(t, err)

	r1, n, err := unmarshalTLogRecord(got)
	require.NoError(t, err)
	r2, _, err := unmarshalTLogRecord(got[n:])
	require.NoError(t, err)
	assert.Equal(t, uint64(1), r1.Seq)
	assert.Equal(t, uint64(2), r2.Seq)
}

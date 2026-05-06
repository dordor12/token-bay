package admission

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTLogReader_ReadsAllRecords(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.tlog")
	w, err := newTLogWriter(path, 5*time.Millisecond, 1<<30)
	require.NoError(t, err)

	for i := uint64(1); i <= 5; i++ {
		require.NoError(t, w.Append(TLogRecord{Seq: i, Ts: 100, Kind: TLogKindSettlement, Payload: []byte{byte(i)}}))
	}
	require.NoError(t, w.Close())

	got, lastGoodOffset, err := readTLogFile(path)
	require.NoError(t, err)
	assert.Len(t, got, 5)
	for i, rec := range got {
		assert.Equal(t, uint64(i+1), rec.Seq)
	}

	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, st.Size(), lastGoodOffset)
}

func TestTLogReader_TruncatedTail_HaltsAtLastGood(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.tlog")
	w, err := newTLogWriter(path, 5*time.Millisecond, 1<<30)
	require.NoError(t, err)
	for i := uint64(1); i <= 3; i++ {
		require.NoError(t, w.Append(TLogRecord{Seq: i, Ts: 100, Kind: TLogKindSettlement, Payload: []byte{byte(i)}}))
	}
	require.NoError(t, w.Close())

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.Write([]byte{0xff, 0xff, 0xff, 0xff, 0xff})
	require.NoError(t, err)
	require.NoError(t, f.Close())

	got, lastGoodOffset, err := readTLogFile(path)
	require.NoError(t, err, "truncated tail is not an error — replay heals it")
	assert.Len(t, got, 3)
	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Less(t, lastGoodOffset, st.Size(), "lastGoodOffset must point before the truncated tail")
}

func TestTLogReader_MidFileCorruption_Errors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.tlog")
	w, err := newTLogWriter(path, 5*time.Millisecond, 1<<30)
	require.NoError(t, err)
	for i := uint64(1); i <= 3; i++ {
		require.NoError(t, w.Append(TLogRecord{Seq: i, Ts: 100, Kind: TLogKindSettlement, Payload: []byte{byte(i)}}))
	}
	require.NoError(t, w.Close())

	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	require.NoError(t, err)
	const offsetIntoSecondRecord = 50
	_, err = f.WriteAt([]byte{0xab}, offsetIntoSecondRecord)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	got, _, err := readTLogFile(path)
	require.ErrorIs(t, err, ErrTLogCorrupt)
	assert.NotEmpty(t, got)
}

func TestEnumerateTLogFiles_OrdersBySeq(t *testing.T) {
	dir := t.TempDir()
	for _, suffix := range []string{".100", ".200", ".50", ""} {
		path := filepath.Join(dir, "admission.tlog"+suffix)
		require.NoError(t, os.WriteFile(path, []byte{}, 0o644))
	}

	files, err := enumerateTLogFiles(filepath.Join(dir, "admission.tlog"))
	require.NoError(t, err)
	require.Len(t, files, 4)
	assert.Equal(t, ".50", filepath.Ext(files[0]))
	assert.Equal(t, ".100", filepath.Ext(files[1]))
	assert.Equal(t, ".200", filepath.Ext(files[2]))
	assert.Equal(t, "admission.tlog", filepath.Base(files[3]), "active file last")
}

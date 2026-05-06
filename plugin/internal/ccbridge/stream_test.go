package ccbridge

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "stream", name))
	require.NoError(t, err)
	return b
}

func TestParseStreamJSON_ResultOnly_ExtractsUsage(t *testing.T) {
	src := loadFixture(t, "result_only.jsonl")
	var sink bytes.Buffer

	usage, err := ParseStreamJSON(bytes.NewReader(src), &sink)
	require.NoError(t, err)
	assert.Equal(t, uint64(12), usage.InputTokens)
	assert.Equal(t, uint64(4), usage.OutputTokens)
	assert.Equal(t, string(src), sink.String())
}

func TestParseStreamJSON_FullSession_UsesLastResultUsage(t *testing.T) {
	src := loadFixture(t, "full_session.jsonl")
	var sink bytes.Buffer

	usage, err := ParseStreamJSON(bytes.NewReader(src), &sink)
	require.NoError(t, err)
	assert.Equal(t, uint64(42), usage.InputTokens)
	assert.Equal(t, uint64(17), usage.OutputTokens)
	assert.Equal(t, string(src), sink.String())
}

func TestParseStreamJSON_NoResult_ReturnsErrNoResult(t *testing.T) {
	src := loadFixture(t, "no_usage.jsonl")
	var sink bytes.Buffer

	_, err := ParseStreamJSON(bytes.NewReader(src), &sink)
	assert.True(t, errors.Is(err, ErrNoResult), "expected ErrNoResult, got %v", err)
	assert.Equal(t, string(src), sink.String())
}

func TestParseStreamJSON_MalformedLine_IsTolerated(t *testing.T) {
	src := loadFixture(t, "malformed_line.jsonl")
	var sink bytes.Buffer

	usage, err := ParseStreamJSON(bytes.NewReader(src), &sink)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), usage.InputTokens)
	assert.Equal(t, uint64(2), usage.OutputTokens)
	assert.Equal(t, string(src), sink.String())
}

type erroringWriter struct {
	err error
}

func (w *erroringWriter) Write(_ []byte) (int, error) { return 0, w.err }

func TestParseStreamJSON_SinkError_Propagates(t *testing.T) {
	src := loadFixture(t, "result_only.jsonl")
	wantErr := errors.New("boom")

	_, err := ParseStreamJSON(bytes.NewReader(src), &erroringWriter{err: wantErr})
	require.Error(t, err)
	assert.True(t, errors.Is(err, wantErr), "expected boom in chain, got %v", err)
}

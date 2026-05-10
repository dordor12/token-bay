package ssetranslate

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadPair returns (stream-json input, expected SSE output) for the
// fixture pair under testdata/sj/<name>.jsonl + testdata/sse/<name>.sse.
func loadPair(t *testing.T, name string) ([]byte, []byte) {
	t.Helper()
	in, err := os.ReadFile(filepath.Join("testdata", "sj", name+".jsonl"))
	require.NoError(t, err, "load input %q", name)
	out, err := os.ReadFile(filepath.Join("testdata", "sse", name+".sse"))
	require.NoError(t, err, "load expected %q", name)
	return in, out
}

// runFixture pumps the stream-json through a Writer one chunk at a time
// (whole file as one Write here; per-line splitting is exercised in a
// dedicated property test) and returns the captured SSE bytes.
func runFixture(t *testing.T, in []byte) []byte {
	t.Helper()
	var sink bytes.Buffer
	w := NewWriter(&sink)
	n, err := w.Write(in)
	require.NoError(t, err)
	assert.Equal(t, len(in), n)
	require.NoError(t, w.Close())
	return sink.Bytes()
}

func TestTranslate_TextOnlySingleCycle(t *testing.T) {
	in, want := loadPair(t, "text_only_single_cycle")
	got := runFixture(t, in)
	assert.Equal(t, string(want), string(got))
}

func TestTranslate_MultiBlock_TextThenToolUse(t *testing.T) {
	in, want := loadPair(t, "multi_block_text_text")
	got := runFixture(t, in)
	assert.Equal(t, string(want), string(got))
}

func TestTranslate_ToolUseThenText(t *testing.T) {
	in, want := loadPair(t, "tool_use_then_text")
	got := runFixture(t, in)
	assert.Equal(t, string(want), string(got))
}

func TestTranslate_MultiCycle_Collapsed(t *testing.T) {
	in, want := loadPair(t, "multi_cycle_collapsed")
	got := runFixture(t, in)
	assert.Equal(t, string(want), string(got))
}

func TestTranslate_MalformedMidStream_Tolerated(t *testing.T) {
	in, want := loadPair(t, "malformed_mid_stream")
	got := runFixture(t, in)
	assert.Equal(t, string(want), string(got))
}

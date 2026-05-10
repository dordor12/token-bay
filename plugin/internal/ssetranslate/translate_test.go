package ssetranslate

import (
	"bytes"
	"errors"
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

func TestTranslate_NoResultEvent_CloseSynthesizes(t *testing.T) {
	in, want := loadPair(t, "no_result_event")
	got := runFixture(t, in)
	assert.Equal(t, string(want), string(got))
}

// errAt returns a writer that fails on the Nth Write call.
type errAt struct {
	wrap   *bytes.Buffer
	failOn int
	calls  int
	err    error
}

func (e *errAt) Write(p []byte) (int, error) {
	e.calls++
	if e.calls == e.failOn {
		return 0, e.err
	}
	return e.wrap.Write(p)
}

func TestTranslate_ChunkBoundaryIndependence(t *testing.T) {
	// Feed the same input one byte at a time; output must be identical
	// to a one-shot Write of the whole input.
	in, _ := loadPair(t, "text_only_single_cycle")

	var oneShot bytes.Buffer
	w1 := NewWriter(&oneShot)
	_, err := w1.Write(in)
	require.NoError(t, err)
	require.NoError(t, w1.Close())

	var oneByte bytes.Buffer
	w2 := NewWriter(&oneByte)
	for _, b := range in {
		n, err := w2.Write([]byte{b})
		require.NoError(t, err)
		require.Equal(t, 1, n)
	}
	require.NoError(t, w2.Close())

	assert.Equal(t, oneShot.String(), oneByte.String())
}

func TestTranslate_InnerWriteError_Latches(t *testing.T) {
	in, _ := loadPair(t, "text_only_single_cycle")
	wantErr := errors.New("boom")
	sink := &errAt{wrap: &bytes.Buffer{}, failOn: 2, err: wantErr}
	w := NewWriter(sink)

	// First Write triggers fail on the second emit (content_block_start
	// after message_start).
	_, err := w.Write(in)
	require.Error(t, err)
	assert.True(t, errors.Is(err, wantErr))

	// Subsequent Write returns the latched error and writes nothing.
	n, err := w.Write([]byte("x"))
	require.Error(t, err)
	assert.Equal(t, 0, n)
	assert.True(t, errors.Is(err, wantErr))

	// Close returns nil because the error is already latched.
	require.NoError(t, w.Close())
}

func TestTranslate_DoubleClose_Idempotent(t *testing.T) {
	in, _ := loadPair(t, "text_only_single_cycle")
	var sink bytes.Buffer
	w := NewWriter(&sink)
	_, err := w.Write(in)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	require.NoError(t, w.Close())
}

func TestTranslate_WriteAfterClose_Errors(t *testing.T) {
	var sink bytes.Buffer
	w := NewWriter(&sink)
	require.NoError(t, w.Close())
	_, err := w.Write([]byte(`{"type":"system"}` + "\n"))
	require.Error(t, err)
}

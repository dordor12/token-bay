package auditlog

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRead_EmptyFileYieldsNothing(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	require.NoError(t, os.WriteFile(p, nil, 0o600))

	var n int
	for range Read(p) {
		n++
	}
	assert.Equal(t, 0, n)
}

func TestRead_ConsumerThenSeederInOrder(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(p)
	require.NoError(t, err)
	now := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	require.NoError(t, l.LogConsumer(ConsumerRecord{RequestID: "c1", Timestamp: now}))
	require.NoError(t, l.LogSeeder(SeederRecord{
		RequestID:      "s1",
		Model:          "claude-haiku-4-5",
		ConsumerIDHash: mustHash32(t, "ab"),
		StartedAt:      now,
		CompletedAt:    now.Add(time.Second),
	}))
	require.NoError(t, l.Close())

	var got []Record
	for rec, err := range Read(p) {
		require.NoError(t, err)
		got = append(got, rec)
	}
	require.Len(t, got, 2)
	c, ok := got[0].(ConsumerRecord)
	require.True(t, ok)
	assert.Equal(t, "c1", c.RequestID)
	s, ok := got[1].(SeederRecord)
	require.True(t, ok)
	assert.Equal(t, "s1", s.RequestID)
}

func TestRead_UnknownKindYieldsUnknownRecord(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	require.NoError(t, os.WriteFile(p, []byte(`{"kind":"future_kind","x":1}`+"\n"), 0o600))

	var got []Record
	for rec, err := range Read(p) {
		require.NoError(t, err)
		got = append(got, rec)
	}
	require.Len(t, got, 1)
	u, ok := got[0].(UnknownRecord)
	require.True(t, ok)
	assert.Equal(t, "future_kind", u.Kind)
}

func TestRead_CorruptLineYieldsErrorAndContinues(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	content := `{"kind":"consumer","request_id":"good1","served_locally":false,"cost_credits":0,"timestamp":"2026-05-07T10:00:00Z"}` + "\n" +
		"not-json" + "\n" +
		`{"kind":"consumer","request_id":"good2","served_locally":false,"cost_credits":0,"timestamp":"2026-05-07T10:01:00Z"}` + "\n"
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))

	var goods, errs int
	for rec, err := range Read(p) {
		if err != nil {
			errs++
			continue
		}
		_, ok := rec.(ConsumerRecord)
		assert.True(t, ok)
		goods++
	}
	assert.Equal(t, 2, goods)
	assert.Equal(t, 1, errs)
}

func TestRead_MissingFileYieldsOneError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nope.log")

	var calls int
	var firstErr error
	for rec, err := range Read(p) {
		calls++
		assert.Nil(t, rec)
		firstErr = err
	}
	assert.Equal(t, 1, calls)
	require.Error(t, firstErr)
}

func TestRead_EarlyBreakStopsIteration(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(p)
	require.NoError(t, err)
	for i := range 5 {
		require.NoError(t, l.LogConsumer(ConsumerRecord{
			RequestID: "c" + strconv.Itoa(i),
			Timestamp: time.Now().UTC(),
		}))
	}
	require.NoError(t, l.Close())

	var n int
	for range Read(p) {
		n++
		break
	}
	assert.Equal(t, 1, n)
}

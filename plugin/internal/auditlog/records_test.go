package auditlog

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarshalRecord_Consumer_RoundTrip(t *testing.T) {
	rec := ConsumerRecord{
		RequestID:     "req-abc",
		ServedLocally: false,
		SeederID:      "seed-1",
		CostCredits:   42,
		Timestamp:     time.Date(2026, 5, 7, 10, 30, 0, 123_456_789, time.UTC),
	}

	line, err := marshalRecord(rec)
	require.NoError(t, err)

	got, err := unmarshalRecord(line)
	require.NoError(t, err)
	require.IsType(t, ConsumerRecord{}, got)
	c := got.(ConsumerRecord)
	assert.Equal(t, rec.RequestID, c.RequestID)
	assert.Equal(t, rec.ServedLocally, c.ServedLocally)
	assert.Equal(t, rec.SeederID, c.SeederID)
	assert.Equal(t, rec.CostCredits, c.CostCredits)
	assert.True(t, rec.Timestamp.Equal(c.Timestamp))
}

func TestMarshalRecord_Consumer_ServedLocallyOmitsSeeder(t *testing.T) {
	rec := ConsumerRecord{
		RequestID:     "req-local",
		ServedLocally: true,
		Timestamp:     time.Date(2026, 5, 7, 10, 30, 0, 0, time.UTC),
	}

	line, err := marshalRecord(rec)
	require.NoError(t, err)

	// No seeder_id key in the wire form.
	assert.NotContains(t, string(line), "seeder_id")
	assert.Contains(t, string(line), `"kind":"consumer"`)
	assert.Contains(t, string(line), `"served_locally":true`)
}

func TestMarshalRecord_Seeder_RoundTrip(t *testing.T) {
	chash := mustHash32(t, "ab")
	thash := mustHash32(t, "cd")
	rec := SeederRecord{
		RequestID:        "req-seed",
		Model:            "claude-sonnet-4-6",
		InputTokens:      120,
		OutputTokens:     340,
		ConsumerIDHash:   chash,
		StartedAt:        time.Date(2026, 5, 7, 10, 30, 0, 0, time.UTC),
		CompletedAt:      time.Date(2026, 5, 7, 10, 30, 5, 0, time.UTC),
		TrackerEntryHash: &thash,
	}

	line, err := marshalRecord(rec)
	require.NoError(t, err)

	got, err := unmarshalRecord(line)
	require.NoError(t, err)
	require.IsType(t, SeederRecord{}, got)
	s := got.(SeederRecord)
	assert.Equal(t, rec.RequestID, s.RequestID)
	assert.Equal(t, rec.Model, s.Model)
	assert.Equal(t, rec.InputTokens, s.InputTokens)
	assert.Equal(t, rec.OutputTokens, s.OutputTokens)
	assert.Equal(t, rec.ConsumerIDHash, s.ConsumerIDHash)
	assert.True(t, rec.StartedAt.Equal(s.StartedAt))
	assert.True(t, rec.CompletedAt.Equal(s.CompletedAt))
	require.NotNil(t, s.TrackerEntryHash)
	assert.Equal(t, *rec.TrackerEntryHash, *s.TrackerEntryHash)
}

func TestMarshalRecord_Seeder_OmitsTrackerHashWhenNil(t *testing.T) {
	rec := SeederRecord{
		RequestID:      "req-no-tracker",
		Model:          "claude-haiku-4-5",
		ConsumerIDHash: mustHash32(t, "11"),
		StartedAt:      time.Date(2026, 5, 7, 10, 30, 0, 0, time.UTC),
		CompletedAt:    time.Date(2026, 5, 7, 10, 30, 1, 0, time.UTC),
	}

	line, err := marshalRecord(rec)
	require.NoError(t, err)
	assert.NotContains(t, string(line), "tracker_entry_hash")
}

func TestUnmarshalRecord_UnknownKindReturnsUnknownRecord(t *testing.T) {
	line := []byte(`{"kind":"future_kind","weird":42}`)

	got, err := unmarshalRecord(line)

	require.NoError(t, err)
	require.IsType(t, UnknownRecord{}, got)
	u := got.(UnknownRecord)
	assert.Equal(t, "future_kind", u.Kind)
	// Raw should hold the full original line so callers can re-decode later.
	var probe map[string]any
	require.NoError(t, json.Unmarshal(u.Raw, &probe))
	assert.Equal(t, float64(42), probe["weird"])
}

func TestUnmarshalRecord_GarbageReturnsError(t *testing.T) {
	_, err := unmarshalRecord([]byte("not-json"))
	require.Error(t, err)
}

func TestMarshalRecord_OneLineNoNewline(t *testing.T) {
	line, err := marshalRecord(ConsumerRecord{
		RequestID: "x",
		Timestamp: time.Unix(0, 0).UTC(),
	})
	require.NoError(t, err)
	assert.False(t, strings.ContainsRune(string(line), '\n'),
		"marshalRecord must not emit a trailing newline; the writer adds it")
}

func TestMarshalRecord_UnknownRecordIsNotMarshalable(t *testing.T) {
	_, err := marshalRecord(UnknownRecord{Kind: "x"})
	require.Error(t, err)
}

// mustHash32 returns a 32-byte array filled with the byte parsed from s
// (e.g. "ab" → 0xab repeated). Compact way to get distinct hashes for tests.
func mustHash32(t *testing.T, hexByte string) [32]byte {
	t.Helper()
	require.Len(t, hexByte, 2)
	hexStr := strings.Repeat(hexByte, 32)
	h, err := decodeHash32(hexStr)
	require.NoError(t, err)
	return h
}

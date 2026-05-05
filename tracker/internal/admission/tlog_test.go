package admission

import (
	"bytes"
	"hash/crc32"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTLogRecord_RoundTrip(t *testing.T) {
	rec := TLogRecord{
		Seq:     42,
		Ts:      1714000000,
		Kind:    TLogKindSettlement,
		Payload: []byte("hello"),
	}
	buf, err := marshalTLogRecord(rec)
	require.NoError(t, err)

	parsed, n, err := unmarshalTLogRecord(buf)
	require.NoError(t, err)
	assert.Equal(t, len(buf), n)
	assert.Equal(t, rec.Seq, parsed.Seq)
	assert.Equal(t, rec.Ts, parsed.Ts)
	assert.Equal(t, rec.Kind, parsed.Kind)
	assert.Equal(t, rec.Payload, parsed.Payload)
}

func TestTLogRecord_CRC_DetectsCorruption(t *testing.T) {
	rec := TLogRecord{Seq: 1, Ts: 1, Kind: TLogKindDisputeFiled, Payload: []byte("x")}
	buf, err := marshalTLogRecord(rec)
	require.NoError(t, err)

	buf[len(buf)-1] ^= 0xff // tamper crc
	_, _, err = unmarshalTLogRecord(buf)
	assert.ErrorIs(t, err, ErrTLogCorrupt)
}

func TestTLogRecord_TruncatedHeader(t *testing.T) {
	rec := TLogRecord{Seq: 1, Ts: 1, Kind: TLogKindSettlement, Payload: nil}
	buf, err := marshalTLogRecord(rec)
	require.NoError(t, err)

	_, _, err = unmarshalTLogRecord(buf[:3])
	assert.ErrorIs(t, err, ErrTLogTruncated)
}

func TestTLogRecord_TruncatedPayload(t *testing.T) {
	rec := TLogRecord{Seq: 1, Ts: 1, Kind: TLogKindSettlement, Payload: []byte("hello")}
	buf, err := marshalTLogRecord(rec)
	require.NoError(t, err)

	_, _, err = unmarshalTLogRecord(buf[:len(buf)-2])
	assert.ErrorIs(t, err, ErrTLogTruncated)
}

func TestTLogRecord_StreamReader(t *testing.T) {
	r1 := TLogRecord{Seq: 1, Ts: 100, Kind: TLogKindSettlement, Payload: []byte("aaa")}
	r2 := TLogRecord{Seq: 2, Ts: 101, Kind: TLogKindDisputeFiled, Payload: []byte("bb")}
	r3 := TLogRecord{Seq: 3, Ts: 102, Kind: TLogKindOperatorOverride, Payload: []byte("c")}

	var stream bytes.Buffer
	for _, r := range []TLogRecord{r1, r2, r3} {
		b, err := marshalTLogRecord(r)
		require.NoError(t, err)
		stream.Write(b)
	}

	got := []TLogRecord{}
	data := stream.Bytes()
	for len(data) > 0 {
		rec, n, err := unmarshalTLogRecord(data)
		require.NoError(t, err)
		got = append(got, rec)
		data = data[n:]
	}
	assert.Equal(t, []TLogRecord{r1, r2, r3}, got)
}

func TestTLogKind_String(t *testing.T) {
	assert.Equal(t, "settlement", TLogKindSettlement.String())
	assert.Equal(t, "dispute_filed", TLogKindDisputeFiled.String())
	assert.Equal(t, "dispute_resolved", TLogKindDisputeResolved.String())
	assert.Equal(t, "heartbeat_bucket_roll", TLogKindHeartbeatBucketRoll.String())
	assert.Equal(t, "snapshot_mark", TLogKindSnapshotMark.String())
	assert.Equal(t, "operator_override", TLogKindOperatorOverride.String())
}

func TestCRC32C_MatchesCastagnoliTable(t *testing.T) {
	stdTable := crc32.MakeTable(crc32.Castagnoli)
	for i := 0; i < 256; i++ {
		assert.Equal(t, stdTable[i], crc32cTable[i], "byte %d", i)
	}
}

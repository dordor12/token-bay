package admission

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// TLogKind classifies an admission.tlog record per admission-design §4.3.
type TLogKind uint8

const (
	// TLogKindSettlement: usage entry finalized.
	TLogKindSettlement TLogKind = 0
	// TLogKindDisputeFiled: dispute opened. Synchronous fsync.
	TLogKindDisputeFiled TLogKind = 1
	// TLogKindDisputeResolved: dispute closed (status: UPHELD or REJECTED).
	// Synchronous fsync.
	TLogKindDisputeResolved TLogKind = 2
	// TLogKindHeartbeatBucketRoll: per-seeder rolling-window roll event.
	// Soft state; batched fsync acceptable.
	TLogKindHeartbeatBucketRoll TLogKind = 3
	// TLogKindSnapshotMark: pointer to a snapshot file emitted at this seq.
	// Replay uses these to find the snapshot start point.
	TLogKindSnapshotMark TLogKind = 4
	// TLogKindOperatorOverride: admin-API mutation. Carries operator
	// identity + parameters in payload.
	TLogKindOperatorOverride TLogKind = 5
	// TLogKindTransfer: peer-region credit transfer (in or out, encoded in
	// the payload's Direction field).
	TLogKindTransfer TLogKind = 6
	// TLogKindStarterGrant: initial credit grant for a new consumer.
	TLogKindStarterGrant TLogKind = 7
	// TLogKindDispute is an alias used by Task 8/persistEvent which routes
	// both filed and (upheld) resolved disputes through one kind. The
	// payload carries the Upheld bit.
	TLogKindDispute = TLogKindDisputeFiled
)

// String returns the canonical lowercase name for a kind. Used in metrics
// labels and operator log lines.
func (k TLogKind) String() string {
	switch k {
	case TLogKindSettlement:
		return "settlement"
	case TLogKindDisputeFiled:
		return "dispute_filed"
	case TLogKindDisputeResolved:
		return "dispute_resolved"
	case TLogKindHeartbeatBucketRoll:
		return "heartbeat_bucket_roll"
	case TLogKindSnapshotMark:
		return "snapshot_mark"
	case TLogKindOperatorOverride:
		return "operator_override"
	case TLogKindTransfer:
		return "transfer"
	case TLogKindStarterGrant:
		return "starter_grant"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(k))
	}
}

// TLogRecord is one durably-written admission event. Spec admission-design §4.3.
type TLogRecord struct {
	Seq     uint64
	Ts      uint64
	Kind    TLogKind
	Payload []byte
}

// crc32cTable is the Castagnoli (CRC32C) table. Computed once at init
// time; used by every read/write.
var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// Sentinels returned by unmarshalTLogRecord. Replay distinguishes
// truncation (likely a partial trailing record at file end) from
// corruption (CRC mismatch on a complete frame) so it can heal the
// tail vs. surface a real bug.
var (
	ErrTLogCorrupt   = errors.New("admission/tlog: CRC32C mismatch")
	ErrTLogTruncated = errors.New("admission/tlog: record truncated")
)

// frame layout: length(4) | seq(8) | ts(8) | kind(1) | payload | crc(4)
const (
	tlogLengthOffset  = 0
	tlogSeqOffset     = 4
	tlogTsOffset      = 12
	tlogKindOffset    = 20
	tlogPayloadOffset = 21                // payload starts here
	tlogMinFrameSize  = 4 + 8 + 8 + 1 + 4 // length + seq + ts + kind + crc
)

// marshalTLogRecord serializes rec into its on-disk frame.
func marshalTLogRecord(rec TLogRecord) ([]byte, error) {
	bodyLen := 8 + 8 + 1 + len(rec.Payload) + 4 // seq + ts + kind + payload + crc
	buf := make([]byte, 4+bodyLen)
	binary.BigEndian.PutUint32(buf[tlogLengthOffset:], uint32(bodyLen)) //nolint:gosec // G115 — bodyLen < 1<<30 (rotation cap)
	binary.BigEndian.PutUint64(buf[tlogSeqOffset:], rec.Seq)
	binary.BigEndian.PutUint64(buf[tlogTsOffset:], rec.Ts)
	buf[tlogKindOffset] = uint8(rec.Kind)
	copy(buf[tlogPayloadOffset:], rec.Payload)

	// CRC over seq | ts | kind | payload (everything from offset 4 up to where crc starts).
	crcStart := tlogSeqOffset
	crcEnd := tlogPayloadOffset + len(rec.Payload)
	crc := crc32.Checksum(buf[crcStart:crcEnd], crc32cTable)
	binary.BigEndian.PutUint32(buf[crcEnd:], crc)
	return buf, nil
}

// unmarshalTLogRecord reads one frame from the start of buf. Returns the
// parsed record, the number of bytes consumed, and an error.
//   - ErrTLogTruncated when buf is shorter than the declared frame
//   - ErrTLogCorrupt when the trailing CRC32C does not match
func unmarshalTLogRecord(buf []byte) (TLogRecord, int, error) {
	if len(buf) < tlogMinFrameSize {
		return TLogRecord{}, 0, ErrTLogTruncated
	}
	bodyLen := int(binary.BigEndian.Uint32(buf[tlogLengthOffset:]))
	frameLen := 4 + bodyLen
	if frameLen > len(buf) {
		return TLogRecord{}, 0, ErrTLogTruncated
	}
	if bodyLen < tlogMinFrameSize-4 {
		return TLogRecord{}, 0, ErrTLogTruncated
	}

	payloadEnd := frameLen - 4
	gotCRC := binary.BigEndian.Uint32(buf[payloadEnd:frameLen])
	wantCRC := crc32.Checksum(buf[tlogSeqOffset:payloadEnd], crc32cTable)
	if gotCRC != wantCRC {
		return TLogRecord{}, 0, ErrTLogCorrupt
	}

	rec := TLogRecord{
		Seq:     binary.BigEndian.Uint64(buf[tlogSeqOffset:]),
		Ts:      binary.BigEndian.Uint64(buf[tlogTsOffset:]),
		Kind:    TLogKind(buf[tlogKindOffset]),
		Payload: append([]byte(nil), buf[tlogPayloadOffset:payloadEnd]...),
	}
	return rec, frameLen, nil
}

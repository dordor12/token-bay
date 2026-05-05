package admission

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	"github.com/token-bay/token-bay/shared/ids"
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

// SettlementPayload is the on-disk form of a TLogKindSettlement record.
type SettlementPayload struct {
	ConsumerID  ids.IdentityID
	SeederID    ids.IdentityID
	CostCredits uint64
	Flags       uint32 // bit 0 = consumer_sig_missing
}

// MarshalBinary returns the on-disk byte form. Implements the io interface
// the writer wraps to keep persistEvent (Task 8) generic.
func (p SettlementPayload) MarshalBinary() ([]byte, error) { return marshalSettlementPayload(p) }

// UnmarshalBinary populates p from buf.
func (p *SettlementPayload) UnmarshalBinary(buf []byte) error {
	got, err := unmarshalSettlementPayload(buf)
	if err != nil {
		return err
	}
	*p = got
	return nil
}

func marshalSettlementPayload(p SettlementPayload) ([]byte, error) {
	buf := make([]byte, 32+32+8+4)
	copy(buf[0:32], p.ConsumerID[:])
	copy(buf[32:64], p.SeederID[:])
	binary.BigEndian.PutUint64(buf[64:72], p.CostCredits)
	binary.BigEndian.PutUint32(buf[72:76], p.Flags)
	return buf, nil
}

func unmarshalSettlementPayload(buf []byte) (SettlementPayload, error) {
	if len(buf) < 32+32+8+4 {
		return SettlementPayload{}, fmt.Errorf("admission/tlog: settlement payload short: %d bytes", len(buf))
	}
	var p SettlementPayload
	copy(p.ConsumerID[:], buf[0:32])
	copy(p.SeederID[:], buf[32:64])
	p.CostCredits = binary.BigEndian.Uint64(buf[64:72])
	p.Flags = binary.BigEndian.Uint32(buf[72:76])
	return p, nil
}

// DisputePayload covers TLogKindDisputeFiled and TLogKindDisputeResolved.
// Upheld is meaningful only for "resolved"; "filed" carries Upheld=false.
type DisputePayload struct {
	ConsumerID ids.IdentityID
	Upheld     bool
}

// MarshalBinary returns the on-disk form.
func (p DisputePayload) MarshalBinary() ([]byte, error) { return marshalDisputePayload(p) }

// UnmarshalBinary populates p from buf.
func (p *DisputePayload) UnmarshalBinary(buf []byte) error {
	got, err := unmarshalDisputePayload(buf)
	if err != nil {
		return err
	}
	*p = got
	return nil
}

func marshalDisputePayload(p DisputePayload) ([]byte, error) {
	buf := make([]byte, 32+1)
	copy(buf[0:32], p.ConsumerID[:])
	if p.Upheld {
		buf[32] = 1
	}
	return buf, nil
}

func unmarshalDisputePayload(buf []byte) (DisputePayload, error) {
	if len(buf) < 33 {
		return DisputePayload{}, fmt.Errorf("admission/tlog: dispute payload short: %d bytes", len(buf))
	}
	var p DisputePayload
	copy(p.ConsumerID[:], buf[0:32])
	p.Upheld = buf[32] != 0
	return p, nil
}

// SnapshotMarkPayload points at the snapshot file the replay should
// load before this record's seq.
type SnapshotMarkPayload struct {
	SnapshotSeq uint64
	// Path is the on-disk snapshot path (best-effort hint for replay; the
	// authoritative discovery is via enumerateSnapshots).
	Path string
}

// Seq is the field name persistEvent references.
func (p SnapshotMarkPayload) Seq() uint64 { return p.SnapshotSeq }

// MarshalBinary returns the on-disk form.
func (p SnapshotMarkPayload) MarshalBinary() ([]byte, error) {
	return marshalSnapshotMarkPayload(p)
}

// UnmarshalBinary populates p from buf.
func (p *SnapshotMarkPayload) UnmarshalBinary(buf []byte) error {
	got, err := unmarshalSnapshotMarkPayload(buf)
	if err != nil {
		return err
	}
	*p = got
	return nil
}

func marshalSnapshotMarkPayload(p SnapshotMarkPayload) ([]byte, error) {
	out := make([]byte, 0, 8+4+len(p.Path))
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint64(hdr, p.SnapshotSeq)
	out = append(out, hdr...)
	out = appendLenPrefixed(out, []byte(p.Path))
	return out, nil
}

func unmarshalSnapshotMarkPayload(buf []byte) (SnapshotMarkPayload, error) {
	if len(buf) < 8 {
		return SnapshotMarkPayload{}, fmt.Errorf("admission/tlog: snapshot_mark payload short: %d bytes", len(buf))
	}
	p := SnapshotMarkPayload{SnapshotSeq: binary.BigEndian.Uint64(buf[:8])}
	if len(buf) > 8 {
		path, _, err := readLenPrefixed(buf[8:])
		if err != nil {
			return SnapshotMarkPayload{}, fmt.Errorf("snapshot_mark path: %w", err)
		}
		p.Path = string(path)
	}
	return p, nil
}

// OperatorOverridePayload is the on-disk form of TLogKindOperatorOverride.
type OperatorOverridePayload struct {
	OperatorID string
	Action     string
	Params     []byte // opaque JSON
	Ts         int64  // unix seconds — set by writeOperatorOverride
}

// ParamsJSON is an alias for Params used by Task 11's helper.
func (p *OperatorOverridePayload) ParamsJSON() []byte { return p.Params }

// MarshalBinary returns the on-disk form.
func (p OperatorOverridePayload) MarshalBinary() ([]byte, error) {
	return marshalOperatorOverridePayload(p)
}

// UnmarshalBinary populates p from buf.
func (p *OperatorOverridePayload) UnmarshalBinary(buf []byte) error {
	got, err := unmarshalOperatorOverridePayload(buf)
	if err != nil {
		return err
	}
	*p = got
	return nil
}

func marshalOperatorOverridePayload(p OperatorOverridePayload) ([]byte, error) {
	out := make([]byte, 0, 4+len(p.OperatorID)+4+len(p.Action)+4+len(p.Params)+8)
	out = appendLenPrefixed(out, []byte(p.OperatorID))
	out = appendLenPrefixed(out, []byte(p.Action))
	out = appendLenPrefixed(out, p.Params)
	tsBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(tsBuf, uint64(p.Ts)) //nolint:gosec // G115 — post-1970 unix
	out = append(out, tsBuf...)
	return out, nil
}

func unmarshalOperatorOverridePayload(buf []byte) (OperatorOverridePayload, error) {
	op, rest, err := readLenPrefixed(buf)
	if err != nil {
		return OperatorOverridePayload{}, fmt.Errorf("operator_id: %w", err)
	}
	action, rest, err := readLenPrefixed(rest)
	if err != nil {
		return OperatorOverridePayload{}, fmt.Errorf("action: %w", err)
	}
	params, rest, err := readLenPrefixed(rest)
	if err != nil {
		return OperatorOverridePayload{}, fmt.Errorf("params: %w", err)
	}
	out := OperatorOverridePayload{
		OperatorID: string(op),
		Action:     string(action),
		Params:     params,
	}
	if len(rest) >= 8 {
		out.Ts = int64(binary.BigEndian.Uint64(rest[:8])) //nolint:gosec // G115 — post-1970 unix
	}
	return out, nil
}

func appendLenPrefixed(dst, val []byte) []byte {
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, uint32(len(val))) //nolint:gosec // G115 — len bounded by frame cap
	dst = append(dst, hdr...)
	return append(dst, val...)
}

func readLenPrefixed(buf []byte) ([]byte, []byte, error) {
	if len(buf) < 4 {
		return nil, nil, fmt.Errorf("admission/tlog: length-prefix truncated")
	}
	n := int(binary.BigEndian.Uint32(buf[:4]))
	if 4+n > len(buf) {
		return nil, nil, fmt.Errorf("admission/tlog: length-prefixed payload truncated")
	}
	return buf[4 : 4+n], buf[4+n:], nil
}

// TransferDirection encodes the sign of a transfer payload.
type TransferDirection uint8

const (
	// TransferIn = consumer received credits.
	TransferIn TransferDirection = 1
	// TransferOut = consumer sent credits.
	TransferOut TransferDirection = 2
)

// TransferPayload is the on-disk form of TLogKindTransfer.
type TransferPayload struct {
	ConsumerID  ids.IdentityID
	CostCredits uint64
	Direction   TransferDirection
}

// MarshalBinary returns the on-disk form.
func (p TransferPayload) MarshalBinary() ([]byte, error) { return marshalTransferPayload(p) }

// UnmarshalBinary populates p from buf.
func (p *TransferPayload) UnmarshalBinary(buf []byte) error {
	got, err := unmarshalTransferPayload(buf)
	if err != nil {
		return err
	}
	*p = got
	return nil
}

func marshalTransferPayload(p TransferPayload) ([]byte, error) {
	buf := make([]byte, 32+8+1)
	copy(buf[0:32], p.ConsumerID[:])
	binary.BigEndian.PutUint64(buf[32:40], p.CostCredits)
	buf[40] = uint8(p.Direction)
	return buf, nil
}

func unmarshalTransferPayload(buf []byte) (TransferPayload, error) {
	if len(buf) < 41 {
		return TransferPayload{}, fmt.Errorf("admission/tlog: transfer payload short: %d bytes", len(buf))
	}
	var p TransferPayload
	copy(p.ConsumerID[:], buf[0:32])
	p.CostCredits = binary.BigEndian.Uint64(buf[32:40])
	p.Direction = TransferDirection(buf[40])
	return p, nil
}

// StarterGrantPayload is the on-disk form of TLogKindStarterGrant.
type StarterGrantPayload struct {
	ConsumerID  ids.IdentityID
	CostCredits uint64
}

// MarshalBinary returns the on-disk form.
func (p StarterGrantPayload) MarshalBinary() ([]byte, error) { return marshalStarterGrantPayload(p) }

// UnmarshalBinary populates p from buf.
func (p *StarterGrantPayload) UnmarshalBinary(buf []byte) error {
	got, err := unmarshalStarterGrantPayload(buf)
	if err != nil {
		return err
	}
	*p = got
	return nil
}

func marshalStarterGrantPayload(p StarterGrantPayload) ([]byte, error) {
	buf := make([]byte, 32+8)
	copy(buf[0:32], p.ConsumerID[:])
	binary.BigEndian.PutUint64(buf[32:40], p.CostCredits)
	return buf, nil
}

func unmarshalStarterGrantPayload(buf []byte) (StarterGrantPayload, error) {
	if len(buf) < 40 {
		return StarterGrantPayload{}, fmt.Errorf("admission/tlog: starter_grant payload short: %d bytes", len(buf))
	}
	var p StarterGrantPayload
	copy(p.ConsumerID[:], buf[0:32])
	p.CostCredits = binary.BigEndian.Uint64(buf[32:40])
	return p, nil
}

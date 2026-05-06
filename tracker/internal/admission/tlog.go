package admission

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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

// tlogWriter manages the active admission.tlog file: batched fsync for
// soft-state kinds, synchronous fsync for disputes, and size-triggered
// rotation. All exported methods (Append, Close) are safe for concurrent
// callers; the internal flushLoop fsync-batches behind the same mutex.
type tlogWriter struct {
	mu            sync.Mutex
	f             *os.File
	bw            *bufio.Writer
	path          string
	rotationBytes int64
	bytesWritten  int64
	lastSeqInFile uint64
	flushInterval time.Duration
	stopCh        chan struct{}
	wg            sync.WaitGroup
}

// newTLogWriter opens (or creates) the active tlog file. flushInterval
// matches admission-design §4.3 ("Batched fsync every 5 ms"); rotationBytes
// is 1 GiB in production, smaller in tests.
func newTLogWriter(path string, flushInterval time.Duration, rotationBytes int64) (*tlogWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("admission/tlog: open %s: %w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	w := &tlogWriter{
		f:             f,
		bw:            bufio.NewWriterSize(f, 4096),
		path:          path,
		rotationBytes: rotationBytes,
		bytesWritten:  st.Size(),
		flushInterval: flushInterval,
		stopCh:        make(chan struct{}),
	}
	w.wg.Add(1)
	go w.flushLoop()
	return w, nil
}

// Append serializes rec and writes it to the active file. Disputes get
// flush + Sync synchronously; everything else is buffered and the flushLoop
// goroutine drives the periodic Sync.
func (w *tlogWriter) Append(rec TLogRecord) error {
	frame, err := marshalTLogRecord(rec)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	if _, err := w.bw.Write(frame); err != nil {
		return fmt.Errorf("admission/tlog: write: %w", err)
	}
	w.bytesWritten += int64(len(frame))
	if rec.Seq > w.lastSeqInFile {
		w.lastSeqInFile = rec.Seq
	}

	if rec.Kind == TLogKindDisputeFiled || rec.Kind == TLogKindDisputeResolved {
		if err := w.bw.Flush(); err != nil {
			return fmt.Errorf("admission/tlog: flush dispute: %w", err)
		}
		if err := w.f.Sync(); err != nil {
			return fmt.Errorf("admission/tlog: sync dispute: %w", err)
		}
	}

	if w.bytesWritten >= w.rotationBytes {
		if err := w.rotateLocked(); err != nil {
			return fmt.Errorf("admission/tlog: rotate: %w", err)
		}
	}
	return nil
}

// LastSeq returns the highest sequence number observed by Append. Used by
// snapshot emitter to stamp the snapshot file (Task 6).
func (w *tlogWriter) LastSeq() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastSeqInFile
}

// Close flushes pending bytes, fsyncs, stops the flush goroutine, and
// closes the active file. Safe to call multiple times.
func (w *tlogWriter) Close() error {
	select {
	case <-w.stopCh:
		return nil
	default:
		close(w.stopCh)
	}
	w.wg.Wait()

	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.bw.Flush(); err != nil {
		return err
	}
	if err := w.f.Sync(); err != nil {
		return err
	}
	return w.f.Close()
}

func (w *tlogWriter) flushLoop() {
	defer w.wg.Done()
	t := time.NewTicker(w.flushInterval)
	defer t.Stop()
	for {
		select {
		case <-w.stopCh:
			return
		case <-t.C:
			w.mu.Lock()
			_ = w.bw.Flush()
			_ = w.f.Sync()
			w.mu.Unlock()
		}
	}
}

// readTLogFile reads every TLogRecord from the file at path. Returns the
// records in file order, the byte offset of the last successfully-parsed
// record's end (so callers can truncate any trailing garbage), and an
// error.
//
// Truncated trailing record (ErrTLogTruncated) is NOT propagated as an
// error — it's the expected post-crash state. Caller treats lastGoodOffset
// as authoritative and (optionally) truncates the file there.
//
// Mid-file corruption (ErrTLogCorrupt) IS propagated; replay halts and
// surfaces the error to the operator (admission-design §7.2).
func readTLogFile(path string) (records []TLogRecord, lastGoodOffset int64, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	var pos int
	for pos < len(data) {
		rec, n, err := unmarshalTLogRecord(data[pos:])
		switch {
		case errors.Is(err, ErrTLogTruncated):
			return records, int64(pos), nil
		case errors.Is(err, ErrTLogCorrupt):
			return records, int64(pos), err
		case err != nil:
			return records, int64(pos), err
		}
		records = append(records, rec)
		pos += n
	}
	return records, int64(pos), nil
}

// enumerateTLogFiles returns every tlog file (rotated + active) for a
// given base path, in seq-ascending order with the active file last.
//
// Naming convention:
//   - rotated: <basePath>.<lastSeqInFile>
//   - active:  <basePath>
//
// Suffixes that fail to parse as uint64 are ignored (defensive against
// unrelated files in the directory).
func enumerateTLogFiles(basePath string) ([]string, error) {
	dir := filepath.Dir(basePath)
	base := filepath.Base(basePath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	type rotated struct {
		path string
		seq  uint64
	}
	var rotateds []rotated
	var hasActive bool

	for _, e := range entries {
		name := e.Name()
		if name == base {
			hasActive = true
			continue
		}
		if !strings.HasPrefix(name, base+".") {
			continue
		}
		suffix := strings.TrimPrefix(name, base+".")
		seq, err := strconv.ParseUint(suffix, 10, 64)
		if err != nil {
			continue
		}
		rotateds = append(rotateds, rotated{path: filepath.Join(dir, name), seq: seq})
	}
	sort.Slice(rotateds, func(i, j int) bool { return rotateds[i].seq < rotateds[j].seq })

	out := make([]string, 0, len(rotateds)+1)
	for _, r := range rotateds {
		out = append(out, r.path)
	}
	if hasActive {
		out = append(out, basePath)
	}
	return out, nil
}

// rotateLocked renames the active file and opens a new one. Caller must
// hold w.mu.
func (w *tlogWriter) rotateLocked() error {
	if err := w.bw.Flush(); err != nil {
		return err
	}
	if err := w.f.Sync(); err != nil {
		return err
	}
	if err := w.f.Close(); err != nil {
		return err
	}
	rotatedPath := fmt.Sprintf("%s.%d", w.path, w.lastSeqInFile)
	if err := os.Rename(w.path, rotatedPath); err != nil {
		return err
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	w.f = f
	w.bw = bufio.NewWriterSize(f, 4096)
	w.bytesWritten = 0
	return nil
}

package admission

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// snapshotMagic identifies admission snapshot files. Spec §4.3 names the
// magic "0xADMSNAP1"; we encode that as the four ASCII bytes "ADMS"
// (0x41 'A', 0x44 'D', 0x4D 'M', 0x53 'S'). format_version handles the
// numeric suffix.
const snapshotMagic uint32 = 0x41444D53

// snapshotFormatVersion bumps on any breaking change to the on-disk
// snapshot layout.
const snapshotFormatVersion uint32 = 1

// Snapshot is the in-memory representation of one snapshot file.
type Snapshot struct {
	Seq       uint64
	Ts        time.Time
	Consumers map[ids.IdentityID]*ConsumerCreditState
	Seeders   map[ids.IdentityID]*SeederHeartbeatState
}

// writeSnapshot serializes (seq, ts, consumers, seeders) atomically to
// path. Writes go to <path>.tmp first; on success, fsync + rename to
// <path>. The trailer CRC32C covers the entire body up to (but excluding)
// the trailer itself.
func writeSnapshot(path string, seq uint64, ts time.Time, consumers map[ids.IdentityID]*ConsumerCreditState, seeders map[ids.IdentityID]*SeederHeartbeatState) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(tmp) // no-op if rename succeeded
	}()

	body := encodeSnapshotBody(seq, ts, consumers, seeders)
	crc := crc32.Checksum(body, crc32cTable)
	trailer := make([]byte, 4)
	binary.LittleEndian.PutUint32(trailer, crc)

	if _, err := f.Write(body); err != nil {
		return err
	}
	if _, err := f.Write(trailer); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func encodeSnapshotBody(seq uint64, ts time.Time, consumers map[ids.IdentityID]*ConsumerCreditState, seeders map[ids.IdentityID]*SeederHeartbeatState) []byte {
	var buf []byte
	header := make([]byte, 24)
	binary.LittleEndian.PutUint32(header[0:4], snapshotMagic)
	binary.LittleEndian.PutUint32(header[4:8], snapshotFormatVersion)
	binary.LittleEndian.PutUint64(header[8:16], seq)
	binary.LittleEndian.PutUint64(header[16:24], uint64(ts.Unix())) //nolint:gosec // G115 — post-1970
	buf = append(buf, header...)

	count := make([]byte, 4)
	binary.LittleEndian.PutUint32(count, uint32(len(consumers))) //nolint:gosec // G115 — bounded by region size
	buf = append(buf, count...)
	for id, st := range consumers {
		buf = append(buf, encodeConsumerState(id, st)...)
	}

	binary.LittleEndian.PutUint32(count, uint32(len(seeders))) //nolint:gosec // G115 — bounded by region size
	buf = append(buf, count...)
	for id, st := range seeders {
		buf = append(buf, encodeSeederState(id, st)...)
	}
	return buf
}

func encodeConsumerState(id ids.IdentityID, st *ConsumerCreditState) []byte {
	out := make([]byte, 0, 32+8+8+rollingWindowDays*3*(4+4+4+8))
	out = append(out, id[:]...)
	tsBuf := make([]byte, 8)
	binary.LittleEndian.PutUint64(tsBuf, uint64(st.FirstSeenAt.Unix())) //nolint:gosec // G115 — post-1970
	out = append(out, tsBuf...)
	balBuf := make([]byte, 8)
	binary.LittleEndian.PutUint64(balBuf, uint64(st.LastBalanceSeen)) //nolint:gosec // G115 — int64-as-uint64 round trip
	out = append(out, balBuf...)
	for i := 0; i < rollingWindowDays; i++ {
		out = append(out, encodeDayBucket(st.SettlementBuckets[i])...)
		out = append(out, encodeDayBucket(st.DisputeBuckets[i])...)
		out = append(out, encodeDayBucket(st.FlowBuckets[i])...)
	}
	return out
}

func encodeDayBucket(b DayBucket) []byte {
	out := make([]byte, 4+4+4+8)
	binary.LittleEndian.PutUint32(out[0:4], b.Total)
	binary.LittleEndian.PutUint32(out[4:8], b.A)
	binary.LittleEndian.PutUint32(out[8:12], b.B)
	binary.LittleEndian.PutUint64(out[12:20], uint64(b.DayStamp.Unix())) //nolint:gosec // G115 — post-1970
	return out
}

func encodeSeederState(id ids.IdentityID, st *SeederHeartbeatState) []byte {
	out := make([]byte, 0, 32+heartbeatWindowMinutes*8+8+4+1+8+1)
	out = append(out, id[:]...)
	for i := 0; i < heartbeatWindowMinutes; i++ {
		buf := make([]byte, 8)
		binary.LittleEndian.PutUint32(buf[0:4], st.Buckets[i].Expected)
		binary.LittleEndian.PutUint32(buf[4:8], st.Buckets[i].Actual)
		out = append(out, buf...)
	}
	tsBuf := make([]byte, 8)
	binary.LittleEndian.PutUint64(tsBuf, uint64(st.LastBucketRollAt.Unix())) //nolint:gosec // G115 — post-1970
	out = append(out, tsBuf...)

	hdrBuf := make([]byte, 4)
	binary.LittleEndian.PutUint32(hdrBuf, st.LastHeadroomEstimate)
	out = append(out, hdrBuf...)
	out = append(out, st.LastHeadroomSource)
	tsBuf2 := make([]byte, 8)
	binary.LittleEndian.PutUint64(tsBuf2, uint64(st.LastHeadroomTs.Unix())) //nolint:gosec // G115 — post-1970
	out = append(out, tsBuf2...)
	if st.CanProbeUsage {
		out = append(out, 1)
	} else {
		out = append(out, 0)
	}
	return out
}

// readSnapshot validates magic + format_version + trailer CRC and returns
// the populated Snapshot. Failures return an error that callers
// (StartupReplay) use to fall back to the next-older snapshot.
func readSnapshot(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 24+4 {
		return nil, fmt.Errorf("admission/snapshot: file %s too short", path)
	}
	if magic := binary.LittleEndian.Uint32(data[0:4]); magic != snapshotMagic {
		return nil, fmt.Errorf("admission/snapshot: bad magic 0x%08x in %s", magic, path)
	}
	if v := binary.LittleEndian.Uint32(data[4:8]); v != snapshotFormatVersion {
		return nil, fmt.Errorf("admission/snapshot: unsupported format_version %d in %s", v, path)
	}

	bodyEnd := len(data) - 4
	wantCRC := binary.LittleEndian.Uint32(data[bodyEnd:])
	gotCRC := crc32.Checksum(data[:bodyEnd], crc32cTable)
	if gotCRC != wantCRC {
		return nil, fmt.Errorf("admission/snapshot: trailer CRC mismatch in %s", path)
	}

	snap := &Snapshot{
		Seq:       binary.LittleEndian.Uint64(data[8:16]),
		Ts:        time.Unix(int64(binary.LittleEndian.Uint64(data[16:24])), 0).UTC(), //nolint:gosec // G115 — post-1970
		Consumers: make(map[ids.IdentityID]*ConsumerCreditState),
		Seeders:   make(map[ids.IdentityID]*SeederHeartbeatState),
	}

	pos := 24
	consumerCount := int(binary.LittleEndian.Uint32(data[pos : pos+4]))
	pos += 4
	for i := 0; i < consumerCount; i++ {
		id, st, n, err := decodeConsumerState(data[pos:])
		if err != nil {
			return nil, fmt.Errorf("admission/snapshot: consumer %d: %w", i, err)
		}
		snap.Consumers[id] = st
		pos += n
	}
	seederCount := int(binary.LittleEndian.Uint32(data[pos : pos+4]))
	pos += 4
	for i := 0; i < seederCount; i++ {
		id, st, n, err := decodeSeederState(data[pos:])
		if err != nil {
			return nil, fmt.Errorf("admission/snapshot: seeder %d: %w", i, err)
		}
		snap.Seeders[id] = st
		pos += n
	}
	return snap, nil
}

func decodeConsumerState(buf []byte) (ids.IdentityID, *ConsumerCreditState, int, error) {
	const fixed = 32 + 8 + 8 + rollingWindowDays*3*(4+4+4+8)
	if len(buf) < fixed {
		return ids.IdentityID{}, nil, 0, fmt.Errorf("consumer state truncated")
	}
	var id ids.IdentityID
	copy(id[:], buf[0:32])
	st := &ConsumerCreditState{
		FirstSeenAt:     time.Unix(int64(binary.LittleEndian.Uint64(buf[32:40])), 0).UTC(), //nolint:gosec // G115
		LastBalanceSeen: int64(binary.LittleEndian.Uint64(buf[40:48])),                     //nolint:gosec // G115
	}
	pos := 48
	for i := 0; i < rollingWindowDays; i++ {
		st.SettlementBuckets[i] = decodeDayBucket(buf[pos : pos+20])
		pos += 20
		st.DisputeBuckets[i] = decodeDayBucket(buf[pos : pos+20])
		pos += 20
		st.FlowBuckets[i] = decodeDayBucket(buf[pos : pos+20])
		pos += 20
	}
	return id, st, fixed, nil
}

func decodeDayBucket(buf []byte) DayBucket {
	return DayBucket{
		Total:    binary.LittleEndian.Uint32(buf[0:4]),
		A:        binary.LittleEndian.Uint32(buf[4:8]),
		B:        binary.LittleEndian.Uint32(buf[8:12]),
		DayStamp: time.Unix(int64(binary.LittleEndian.Uint64(buf[12:20])), 0).UTC(), //nolint:gosec // G115
	}
}

func decodeSeederState(buf []byte) (ids.IdentityID, *SeederHeartbeatState, int, error) {
	const fixed = 32 + heartbeatWindowMinutes*8 + 8 + 4 + 1 + 8 + 1
	if len(buf) < fixed {
		return ids.IdentityID{}, nil, 0, fmt.Errorf("seeder state truncated")
	}
	var id ids.IdentityID
	copy(id[:], buf[0:32])
	st := &SeederHeartbeatState{}
	pos := 32
	for i := 0; i < heartbeatWindowMinutes; i++ {
		st.Buckets[i] = MinuteBucket{
			Expected: binary.LittleEndian.Uint32(buf[pos : pos+4]),
			Actual:   binary.LittleEndian.Uint32(buf[pos+4 : pos+8]),
		}
		pos += 8
	}
	st.LastBucketRollAt = time.Unix(int64(binary.LittleEndian.Uint64(buf[pos:pos+8])), 0).UTC() //nolint:gosec // G115
	pos += 8
	st.LastHeadroomEstimate = binary.LittleEndian.Uint32(buf[pos : pos+4])
	pos += 4
	st.LastHeadroomSource = buf[pos]
	pos++
	st.LastHeadroomTs = time.Unix(int64(binary.LittleEndian.Uint64(buf[pos:pos+8])), 0).UTC() //nolint:gosec // G115
	pos += 8
	st.CanProbeUsage = buf[pos] != 0
	return id, st, fixed, nil
}

// snapshotFile is one entry returned by enumerateSnapshots.
type snapshotFile struct {
	path string
	seq  uint64
}

// enumerateSnapshots returns every snapshot file matching <prefix>.<seq>
// in seq-ascending order. Suffixes that fail to parse as uint64 are
// skipped (defensive against unrelated files in the directory).
func enumerateSnapshots(prefix string) ([]snapshotFile, error) {
	dir := filepath.Dir(prefix)
	base := filepath.Base(prefix)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []snapshotFile
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, base+".") {
			continue
		}
		suffix := strings.TrimPrefix(name, base+".")
		if strings.HasSuffix(suffix, ".tmp") {
			continue
		}
		seq, err := strconv.ParseUint(suffix, 10, 64)
		if err != nil {
			continue
		}
		out = append(out, snapshotFile{path: filepath.Join(dir, name), seq: seq})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	return out, nil
}

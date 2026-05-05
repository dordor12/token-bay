# Tracker — `internal/admission` Persistence + Admin + Acceptance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the durability and operations layer for the admission subsystem — `admission.tlog` + periodic snapshots + `StartupReplay` + 11 admin HTTP handlers + ~20 Prometheus metrics — and validate the §10 acceptance criteria #9-20 (persistence/recovery, performance, security, operator auth) that plan 2 explicitly deferred.

**Architecture:** Add three new files to `tracker/internal/admission`: `tlog.go` (CRC32C-framed append-only event log with batched fsync, dispute fdatasync, 1-GiB rotation), `snapshot.go` (binary snapshot + 600s emit goroutine + last-3 retention), `replay.go` (snapshot load → tlog replay → ledger cross-check). Modify `events.go` (plan 2's in-memory `OnLedgerEvent`) to also append a `TLogRecord` per event — disputes get sync, settlements get the 5ms batched window. Add `admin.go` with 11 `http.Handler` instances and a `RegisterMux` helper (admin server itself is a separate plan; we only ship handlers). Add `metrics.go` with a Prometheus `Collector` whoever builds the metrics endpoint can register. Acceptance hardening lands as integration tests in `acceptance_test.go`.

**Tech Stack:** Go 1.23+, stdlib (`hash/crc32` Castagnoli table, `bufio`, `os`, `encoding/binary`, `net/http`, `encoding/json`), `github.com/prometheus/client_golang/prometheus`, `github.com/stretchr/testify`. The Prometheus client is already on `tracker/CLAUDE.md`'s tech stack but not yet imported anywhere — `tracker/go.mod` will gain it on first import.

**Specs:**
- `docs/superpowers/specs/admission/2026-04-25-admission-design.md` — primary. §4.3 (on-disk format), §5.6 (OnLedgerEvent extended w/ tlog append), §5.7 (StartupReplay), §5.8 (SnapshotEmit), §7 (failure handling matrix), §9.1 (admin endpoints), §9.2 (metrics), §10 #9-20 (acceptance).
- `docs/superpowers/specs/tracker/2026-04-22-tracker-design.md` — §6 race-list (admission already in it after plan 1).

**Dependency order:** Runs after **plan 2** (`tracker_admission_core`) merges to `main`. Plan 2 ships the in-memory subsystem this plan adds durability to.

**Branch:** `tracker_admission_persistence` off `main` head (after plan 2 merges). One PR per plan.

**Repo path:** This worktree lives at `/Users/dor.amid/.superset/worktrees/token-bay/tracker_admission`. All absolute paths use that prefix. Module-relative commands run from `/Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker`.

---

## 1. File map

```
tracker/internal/admission/
├── tlog.go                          ← CREATE: TLogRecord + framing + writer + rotation
├── tlog_test.go                     ← CREATE: round-trip, CRC, fsync, rotation
├── snapshot.go                      ← CREATE: SnapshotFile format + emit + load + retention
├── snapshot_test.go                 ← CREATE
├── replay.go                        ← CREATE: StartupReplay + ledger cross-check
├── replay_test.go                   ← CREATE
├── events.go                        ← MODIFY: OnLedgerEvent now appends to tlog
├── events_test.go                   ← MODIFY: add tlog-write assertions to existing tests
├── admin.go                         ← CREATE: 11 http.Handler instances + RegisterMux
├── admin_test.go                    ← CREATE: handler tests + auth gating
├── operator_override.go             ← CREATE: helper for admin-mutating handlers
├── operator_override_test.go        ← CREATE
├── metrics.go                       ← CREATE: Prometheus Collector with ~20 metrics
├── metrics_test.go                  ← CREATE
├── acceptance_test.go               ← CREATE: §10 #9-12 (persistence) + #17-20 (security)
├── perf_bench_test.go               ← CREATE: §10 #13-16 benchmarks
├── degraded_mode.go                 ← CREATE: degraded-mode flag + emergency state rebuild
├── degraded_mode_test.go            ← CREATE
└── (existing files from plan 2)     ← unchanged

tracker/go.mod                       ← MODIFY: add prometheus/client_golang require
```

Notes:
- One Go file per area of responsibility; each test file is scoped to its sibling.
- `acceptance_test.go` houses the cross-component §10 integration tests so they don't pollute per-feature `*_test.go` files.
- `perf_bench_test.go` uses `testing.B` benchmarks; `make check` does not run benchmarks (run with `go test -bench=. -benchtime=10s`).
- `operator_override.go` is a thin shared helper (10-20 lines) so each admin POST/DELETE handler doesn't duplicate the OPERATOR_OVERRIDE TLogRecord construction.
- `degraded_mode.go` houses the `admission_degraded_mode_active` gauge state machine so it isn't tangled into replay's main path.

## 2. Conventions used in this plan

- All `go test`, `go build`, and `make` commands run from `/Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker` unless a different `cd` is shown.
- `PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH"` if Go is managed by mise. Bash steps include it where required.
- One commit per task. Conventional-commit prefixes: `feat(tracker/admission):`, `test(tracker/admission):`, `refactor(tracker/admission):`, `chore(tracker/admission):`.
- Co-Authored-By footer on every commit:
  ```
  Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
  ```
- TDD: failing test first → run-and-confirm-fail → minimal impl → run-and-confirm-pass → commit.
- Lint policy: `make lint` from each module must stay green. Plan 2's nolint patterns (G115 gosec on bounded conversions; G404 on jitter rand; gofumpt formatting) carry forward — apply the same pattern when needed.
- Race discipline: every test invocation uses `-race`. `internal/admission` is on the §6 mandatory race-list.
- Coverage target: ≥ 90% line coverage on every file in `internal/admission/`. Persistence has more error paths than score/decide so target ≥ 88% there if a marshal-error branch is provably unreachable.

## 3. Design decisions used by all tasks

### 3.1 `TLogRecord` wire format (admission-design §4.3)

```
+---------+---------+---------+---------+-----------+---------+
| length  |   seq   |   ts    |  kind   |  payload  |  crc32c |
| 4 bytes | 8 bytes | 8 bytes | 1 byte  | length-21 | 4 bytes |
+---------+---------+---------+---------+-----------+---------+
```

- `length` (uint32, big-endian): total bytes from `seq` start through `crc32c` end. Equals `len(payload) + 8 + 8 + 1 + 4 = len(payload) + 21`.
- `seq` (uint64, big-endian): monotonic per-tlog-file. Resets per file rotation.
- `ts` (uint64, big-endian): unix seconds, tracker wall-clock at write time.
- `kind` (uint8): one of:
  - `0 = TLogKindSettlement`
  - `1 = TLogKindDisputeFiled`
  - `2 = TLogKindDisputeResolved`
  - `3 = TLogKindHeartbeatBucketRoll`
  - `4 = TLogKindSnapshotMark`
  - `5 = TLogKindOperatorOverride`
- `payload`: kind-specific protobuf bytes. Each kind has its own helper marshaller in `tlog.go`.
- `crc32c`: Castagnoli polynomial (`0x82f63b78`) over the concatenation `seq || ts || kind || payload` (21+ bytes, excluding the leading length and the crc itself).

Reads validate `crc32c`; mismatched records halt replay at the last good record (admission-design §7.2).

### 3.2 fsync policy

- **Settlements / transfers / starter_grants** (`TLogKindSettlement` + soft-state kinds): batched. `tlogWriter.write` queues to a 4 KiB buffer; a 5 ms `time.Ticker` flushes the buffer with `Sync()` (= fsync). Crash window: ≤ 5 ms.
- **Disputes** (`TLogKindDisputeFiled`, `TLogKindDisputeResolved`): synchronous. `write` flushes the buffer, then calls `f.Sync()` before returning. Crash window: 0.
- **Snapshot marks + operator overrides**: batched (5 ms) — they piggyback on the next batch flush.

The choice mirrors admission-design §4.3 ("Dispute records use synchronous `fdatasync` for stricter durability — they have no ledger backing to recover from").

### 3.3 File rotation

- Active file: `${cfg.TLogPath}` (e.g. `/var/lib/tracker/admission.tlog`).
- After every successful write, check `f.Stat().Size() ≥ 1<<30` (1 GiB).
- On rotation: close active file, rename to `${cfg.TLogPath}.${last_seq_in_file}`, open fresh active file.
- Old files are **never** deleted (repo rule #4).
- The next file's `seq` continues monotonically; replay reads files in `last_seq` ascending order plus the active one last.

### 3.4 Snapshot file format

```
+---------+----------------+-------+-------+---------------+--------------+----------------+
| magic   | format_version |  seq  |  ts   |  consumers    |  seeders     |  trailer_crc32 |
| 4 bytes |     4 bytes    |8 bytes|8 bytes|varint+repeated|varint+repeated|     4 bytes   |
+---------+----------------+-------+-------+---------------+--------------+----------------+
```

- `magic` = `0xADMSNAP1` (LE).
- `format_version` = `1`.
- `seq` = the tlog seq at which this snapshot was taken (replay knows to start from `seq+1`).
- `ts` = unix seconds.
- `consumers` and `seeders` are length-prefixed repeated entries (varint count + per-entry protobuf).
- `trailer_crc32` is CRC32C over the entire body up to (and excluding) itself.

Atomic write: write to `${cfg.SnapshotPathPrefix}.${seq}.tmp`, fsync, rename to `${cfg.SnapshotPathPrefix}.${seq}`.

### 3.5 ConsumerCreditState / SeederHeartbeatState serialization

Snapshots persist Plan 2's in-memory types. Both are pure-data with no goroutine state. Serialization choice: a simple binary encoding on top of `encoding/binary` (no proto for these — the types are tracker-private, never on the wire). Format documented in `snapshot.go` in Task 4.

### 3.6 Replay decision tree (admission-design §5.7 + §7)

```
Read latest snapshot S (or fall back to next-older on corruption)
    ↓
Open admission.tlog active file + every rotated file
    ↓
For each TLogRecord with seq > S.seq:
    validate CRC; on mismatch → halt at last good seq, surface to operator
    apply to in-memory state (mirror OnLedgerEvent's mutations)
    ↓
Cross-check: query ledger for entries with seq > tlog_max_seq
    For any found, synthesize equivalent TLogRecord (kind = SETTLEMENT / TRANSFER_IN / etc),
    append to tlog, apply to state
    ↓
Mark replay complete; clear admission_degraded_mode_active gauge
```

### 3.7 Admin endpoint authentication

Admin handlers DON'T enforce auth themselves — that's the admin server's middleware in a future plan. Plan 3's handlers expose a `RequireOperatorAuth` interface on the registered Mux: callers wrap the handler with their own auth middleware, or use the included `BasicAuthGuard(ServeMux, validator)` helper that checks the standard tracker operator-token header. §10 #20 acceptance test uses `BasicAuthGuard` to assert 401 without a valid header.

### 3.8 Prometheus metric naming

All metrics start with `admission_` per spec §9.2. Histograms use the prometheus default buckets unless a spec criterion requires custom bucketing — `admission_decision_duration_seconds` uses `[100µs, 250µs, 500µs, 1ms, 2ms, 5ms, 10ms]` because spec §10 #13 measures p99 < 1ms / 2ms.

### 3.9 Out-of-scope reminders (carried from plan 2)

| Concern | Where it lives |
|---|---|
| Admin HTTP server skeleton (listener + auth middleware) | future tracker control-plane plan |
| Real ledger event-bus emission (ledger Notify) | future ledger plan |
| Broker integration | future tracker control-plane plan |
| FetchHeadroom RPC | future tracker↔seeder RPC plan |
| Real federation peer-set lookup | future federation plan |

---

## Task 1: tlog framing + CRC32C

**Files:**
- Create: `tracker/internal/admission/tlog.go`
- Create: `tracker/internal/admission/tlog_test.go`

Adds the `TLogRecord` type, `TLogKind` enum, and the framing helpers `marshalTLogRecord` / `unmarshalTLogRecord`. No I/O yet — pure bytes-in / bytes-out. Task 3 builds the writer on top of these primitives.

- [ ] **Step 1: Write the failing test**

Create `tracker/internal/admission/tlog_test.go`:

```go
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
	// Sanity: the table we use in tlog.go must equal stdlib's Castagnoli table.
	stdTable := crc32.MakeTable(crc32.Castagnoli)
	for i := 0; i < 256; i++ {
		assert.Equal(t, stdTable[i], crc32cTable[i], "byte %d", i)
	}
}
```

- [ ] **Step 2: Run, confirm FAIL**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: compile failure — `TLogRecord`, `TLogKind*`, `marshalTLogRecord`, `unmarshalTLogRecord`, `ErrTLogCorrupt`, `ErrTLogTruncated`, `crc32cTable` undefined.

- [ ] **Step 3: Write the implementation**

Create `tracker/internal/admission/tlog.go`:

```go
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
	tlogPayloadOffset = 21 // payload starts here
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
```

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: all 7 new tests PASS (round-trip, CRC, truncated header, truncated payload, stream, kind String, CRC table sanity).

- [ ] **Step 5: Commit**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
git add tracker/internal/admission/tlog.go tracker/internal/admission/tlog_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/admission): TLogRecord framing + CRC32C

Lands the on-disk frame format from admission-design §4.3:
  length(4) | seq(8) | ts(8) | kind(1) | payload | crc32c(4)

CRC32C uses stdlib hash/crc32 with the Castagnoli polynomial
(0x82f63b78). Tests pin the table against crc32.MakeTable so a future
import-path change can't silently switch us off Castagnoli.

unmarshal returns sentinel ErrTLogTruncated vs ErrTLogCorrupt so replay
can distinguish "trailing partial frame from a crash" from "real CRC
mismatch on a complete frame" — the former heals by truncating to the
last good record; the latter surfaces to the operator.

No I/O yet — pure bytes-in/bytes-out. The writer + rotation lands in
the next task; OnLedgerEvent integration in a follow-up.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: tlog payloads — kind-specific marshalling

**Files:**
- Modify: `tracker/internal/admission/tlog.go`
- Modify: `tracker/internal/admission/tlog_test.go`

Each `TLogKind` carries a kind-specific payload struct. This task adds Go types + marshal/unmarshal helpers for each. Keeping payload encoding here (not in `events.go`) keeps the durability-format definition co-located with the framing.

- [ ] **Step 1: Append failing tests**

Append to `tracker/internal/admission/tlog_test.go`:

```go
import (
	"github.com/token-bay/token-bay/shared/ids"
)
```

(Add to the existing import block.)

```go
func TestSettlementPayload_RoundTrip(t *testing.T) {
	p := SettlementPayload{
		ConsumerID:  makeID(0xC1),
		SeederID:    makeID(0x5E),
		CostCredits: 1234,
		Flags:       0,
	}
	buf, err := marshalSettlementPayload(p)
	require.NoError(t, err)
	got, err := unmarshalSettlementPayload(buf)
	require.NoError(t, err)
	assert.Equal(t, p, got)
}

func TestDisputePayload_RoundTrip(t *testing.T) {
	p := DisputePayload{ConsumerID: makeID(0x42), Upheld: true}
	buf, err := marshalDisputePayload(p)
	require.NoError(t, err)
	got, err := unmarshalDisputePayload(buf)
	require.NoError(t, err)
	assert.Equal(t, p, got)
}

func TestSnapshotMarkPayload_RoundTrip(t *testing.T) {
	p := SnapshotMarkPayload{SnapshotSeq: 9999}
	buf, err := marshalSnapshotMarkPayload(p)
	require.NoError(t, err)
	got, err := unmarshalSnapshotMarkPayload(buf)
	require.NoError(t, err)
	assert.Equal(t, p, got)
}

func TestOperatorOverridePayload_RoundTrip(t *testing.T) {
	p := OperatorOverridePayload{
		OperatorID: "alice@example",
		Action:     "queue_drain",
		Params:     []byte(`{"n":5}`),
	}
	buf, err := marshalOperatorOverridePayload(p)
	require.NoError(t, err)
	got, err := unmarshalOperatorOverridePayload(buf)
	require.NoError(t, err)
	assert.Equal(t, p, got)
}

func TestTransferPayload_RoundTrip(t *testing.T) {
	p := TransferPayload{ConsumerID: makeID(0x11), CostCredits: 500, Direction: TransferIn}
	buf, err := marshalTransferPayload(p)
	require.NoError(t, err)
	got, err := unmarshalTransferPayload(buf)
	require.NoError(t, err)
	assert.Equal(t, p, got)
}

func TestStarterGrantPayload_RoundTrip(t *testing.T) {
	p := StarterGrantPayload{ConsumerID: makeID(0x42), CostCredits: 1000}
	buf, err := marshalStarterGrantPayload(p)
	require.NoError(t, err)
	got, err := unmarshalStarterGrantPayload(buf)
	require.NoError(t, err)
	assert.Equal(t, p, got)
}

func TestUnmarshalShorterThanExpected_Errors(t *testing.T) {
	cases := []struct {
		name string
		fn   func([]byte) error
	}{
		{"settlement", func(b []byte) error { _, err := unmarshalSettlementPayload(b); return err }},
		{"dispute", func(b []byte) error { _, err := unmarshalDisputePayload(b); return err }},
		{"snapshot_mark", func(b []byte) error { _, err := unmarshalSnapshotMarkPayload(b); return err }},
		{"transfer", func(b []byte) error { _, err := unmarshalTransferPayload(b); return err }},
		{"starter_grant", func(b []byte) error { _, err := unmarshalStarterGrantPayload(b); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.fn([]byte{1, 2, 3})
			require.Error(t, err)
		})
	}
}
```

The `makeID` helper is from `helpers_test.go` (plan 2).

- [ ] **Step 2: Run, confirm FAIL**

Same `go test` command. Expected: all the new types undefined.

- [ ] **Step 3: Append the payload types + marshallers**

Append to `tracker/internal/admission/tlog.go`:

```go
import (
	"github.com/token-bay/token-bay/shared/ids"
)
```

(Add `ids` to the existing import block.)

```go
// SettlementPayload is the on-disk form of a TLogKindSettlement record.
// Mirrors the LedgerEvent.Settlement shape so replay can re-apply
// without re-fetching the original ledger entry.
type SettlementPayload struct {
	ConsumerID  ids.IdentityID
	SeederID    ids.IdentityID
	CostCredits uint64
	Flags       uint32 // bit 0 = consumer_sig_missing
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

// DisputePayload is the on-disk form of TLogKindDisputeFiled and
// TLogKindDisputeResolved. Upheld is meaningful only for "resolved";
// "filed" carries Upheld=false.
type DisputePayload struct {
	ConsumerID ids.IdentityID
	Upheld     bool
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
}

func marshalSnapshotMarkPayload(p SnapshotMarkPayload) ([]byte, error) {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, p.SnapshotSeq)
	return buf, nil
}

func unmarshalSnapshotMarkPayload(buf []byte) (SnapshotMarkPayload, error) {
	if len(buf) < 8 {
		return SnapshotMarkPayload{}, fmt.Errorf("admission/tlog: snapshot_mark payload short: %d bytes", len(buf))
	}
	return SnapshotMarkPayload{SnapshotSeq: binary.BigEndian.Uint64(buf)}, nil
}

// OperatorOverridePayload is the on-disk form of TLogKindOperatorOverride.
// Action is a free-form lowercase identifier (queue_drain, snapshot_emit,
// etc); Params is opaque JSON the action handler interprets.
type OperatorOverridePayload struct {
	OperatorID string // e.g. "alice@example"
	Action     string // e.g. "queue_drain"
	Params     []byte // opaque JSON
}

func marshalOperatorOverridePayload(p OperatorOverridePayload) ([]byte, error) {
	// length-prefixed strings then params.
	out := make([]byte, 0, 4+len(p.OperatorID)+4+len(p.Action)+4+len(p.Params))
	out = appendLenPrefixed(out, []byte(p.OperatorID))
	out = appendLenPrefixed(out, []byte(p.Action))
	out = appendLenPrefixed(out, p.Params)
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
	params, _, err := readLenPrefixed(rest)
	if err != nil {
		return OperatorOverridePayload{}, fmt.Errorf("params: %w", err)
	}
	return OperatorOverridePayload{
		OperatorID: string(op),
		Action:     string(action),
		Params:     params,
	}, nil
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

// TransferPayload encodes a TLogKindSettlement-related transfer event.
// Lives in tlog because it's recoverable but not in TLogKindSettlement
// itself (which is for usage). Plan 2's events.go calls applyTransfer for
// these.
type TransferPayload struct {
	ConsumerID  ids.IdentityID
	CostCredits uint64
	Direction   TransferDirection
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

// StarterGrantPayload encodes a TLogKindSettlement-equivalent for grants.
type StarterGrantPayload struct {
	ConsumerID  ids.IdentityID
	CostCredits uint64
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
```

Note: `TLogKindSettlement` covers usage settlements only. Transfer / starter_grant get their own subtypes carried inside the same `TLogKindSettlement` *bucket* — replay reads the kind from the frame, but the payload format depends on which `OnLedgerEvent` produced it. To keep this clean, this task introduces three **new** TLogKinds — append to the existing const block in tlog.go:

```go
const (
	// (existing TLogKindSettlement etc)

	// TLogKindTransfer: TransferPayload (in/out direction inside payload).
	TLogKindTransfer TLogKind = 6
	// TLogKindStarterGrant: StarterGrantPayload.
	TLogKindStarterGrant TLogKind = 7
)
```

Update `String()` to handle the two new kinds.

- [ ] **Step 4: Run, confirm PASS**

Same `go test` command. All payload round-trip + truncation tests PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/admission/tlog.go tracker/internal/admission/tlog_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/admission): tlog payload types per kind

Each TLogKind carries a typed payload — SettlementPayload, DisputePayload,
SnapshotMarkPayload, OperatorOverridePayload, TransferPayload,
StarterGrantPayload. Marshal/unmarshal pairs use big-endian fixed-width
encoding for primitives + length-prefixed bytes for variable-length
fields (operator_id, action, params).

Adds two new TLogKinds (Transfer = 6, StarterGrant = 7) so plan 2's
five OnLedgerEvent kinds map cleanly: SETTLEMENT → TLogKindSettlement;
TRANSFER_IN/OUT → TLogKindTransfer (direction inside payload);
STARTER_GRANT → TLogKindStarterGrant; DISPUTE_FILED → TLogKindDisputeFiled;
DISPUTE_RESOLVED → TLogKindDisputeResolved.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: tlog writer — batched fsync + dispute-sync + rotation

**Files:**
- Modify: `tracker/internal/admission/tlog.go`
- Create: `tracker/internal/admission/tlog_writer_test.go`

Adds `tlogWriter` struct + `Open` / `Append(rec)` / `Close` methods. Append routes by kind:
- Disputes → write + flush + `f.Sync()` synchronously
- Everything else → buffer, returns immediately; 5 ms batched ticker calls `f.Sync()`

Rotation triggers when `Stat().Size() >= 1<<30`.

- [ ] **Step 1: Write the failing tests**

Create `tracker/internal/admission/tlog_writer_test.go`:

```go
package admission

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTLogWriter_AppendThenRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.tlog")
	w, err := newTLogWriter(path, 5*time.Millisecond, 1<<30)
	require.NoError(t, err)
	defer w.Close()

	rec := TLogRecord{Seq: 1, Ts: 100, Kind: TLogKindSettlement, Payload: []byte("hello")}
	require.NoError(t, w.Append(rec))
	require.NoError(t, w.Close())

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	parsed, _, err := unmarshalTLogRecord(got)
	require.NoError(t, err)
	assert.Equal(t, rec, parsed)
}

func TestTLogWriter_DisputeSyncImmediate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.tlog")
	w, err := newTLogWriter(path, 1*time.Hour /*long batch window*/, 1<<30)
	require.NoError(t, err)
	defer w.Close()

	rec := TLogRecord{Seq: 1, Ts: 100, Kind: TLogKindDisputeFiled, Payload: []byte("d1")}
	require.NoError(t, w.Append(rec))

	// Without Close, dispute writes must be readable on disk because
	// they are sync'd synchronously.
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotEmpty(t, got)
}

func TestTLogWriter_BatchedFlushHonorsCadence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.tlog")
	w, err := newTLogWriter(path, 50*time.Millisecond, 1<<30)
	require.NoError(t, err)
	defer w.Close()

	rec := TLogRecord{Seq: 1, Ts: 100, Kind: TLogKindSettlement, Payload: []byte("s1")}
	require.NoError(t, w.Append(rec))

	// Wait at least one flush cadence + slack.
	time.Sleep(150 * time.Millisecond)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotEmpty(t, got)
}

func TestTLogWriter_Rotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.tlog")

	// Tiny rotation cap so a few records trigger it.
	w, err := newTLogWriter(path, 5*time.Millisecond, 200 /*bytes*/)
	require.NoError(t, err)
	defer w.Close()

	for i := uint64(1); i <= 10; i++ {
		require.NoError(t, w.Append(TLogRecord{
			Seq: i, Ts: 100, Kind: TLogKindSettlement, Payload: []byte("x12345678901234567890"),
		}))
	}
	require.NoError(t, w.Close())

	// At least one rotated file exists.
	matches, err := filepath.Glob(path + ".*")
	require.NoError(t, err)
	assert.NotEmpty(t, matches, "rotated files should exist after exceeding the cap")
}

func TestTLogWriter_ConcurrentAppendsRaceClean(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.tlog")
	w, err := newTLogWriter(path, 5*time.Millisecond, 1<<30)
	require.NoError(t, err)
	defer w.Close()

	var wg sync.WaitGroup
	for i := uint64(1); i <= 100; i++ {
		wg.Add(1)
		go func(seq uint64) {
			defer wg.Done()
			err := w.Append(TLogRecord{Seq: seq, Ts: 100, Kind: TLogKindSettlement, Payload: []byte("z")})
			assert.NoError(t, err)
		}(i)
	}
	wg.Wait()
	// Race-detector clean check; explicit Close flushes pending bytes.
	require.NoError(t, w.Close())
}

func TestTLogWriter_OpenAppendsToExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.tlog")

	w1, err := newTLogWriter(path, 5*time.Millisecond, 1<<30)
	require.NoError(t, err)
	require.NoError(t, w1.Append(TLogRecord{Seq: 1, Ts: 100, Kind: TLogKindSettlement, Payload: []byte("a")}))
	require.NoError(t, w1.Close())

	w2, err := newTLogWriter(path, 5*time.Millisecond, 1<<30)
	require.NoError(t, err)
	require.NoError(t, w2.Append(TLogRecord{Seq: 2, Ts: 101, Kind: TLogKindSettlement, Payload: []byte("b")}))
	require.NoError(t, w2.Close())

	got, err := os.ReadFile(path)
	require.NoError(t, err)

	r1, n, err := unmarshalTLogRecord(got)
	require.NoError(t, err)
	r2, _, err := unmarshalTLogRecord(got[n:])
	require.NoError(t, err)
	assert.Equal(t, uint64(1), r1.Seq)
	assert.Equal(t, uint64(2), r2.Seq)
}
```

- [ ] **Step 2: Run, confirm FAIL**

Same `go test` command. Expected: `newTLogWriter`, `tlogWriter.Append`, `tlogWriter.Close` undefined.

- [ ] **Step 3: Append the writer**

Append to `tracker/internal/admission/tlog.go`:

```go
import (
	"bufio"
	"os"
	"sync"
	"time"
)
```

(Add to import block.)

```go
// tlogWriter manages the active admission.tlog file: batched fsync for
// soft-state kinds, synchronous fsync for disputes, and 1-GiB rotation.
//
// All exported methods (Append, Close) are safe for concurrent callers.
// The internal batch flush goroutine never touches the file directly —
// it sends a tick to flushReq, and Append/Close consume from there.
//
// Rotation: when Stat().Size() crosses rotationBytes after a write,
// the active file is renamed to <path>.<last_seq_in_file> and a fresh
// file opens with seq continuing monotonically from the caller's input.
type tlogWriter struct {
	mu             sync.Mutex
	f              *os.File
	bw             *bufio.Writer
	path           string
	rotationBytes  int64
	bytesWritten   int64
	lastSeqInFile  uint64
	flushInterval  time.Duration
	stopCh         chan struct{}
	wg             sync.WaitGroup
}

// newTLogWriter opens (or creates) the active tlog file at path. The
// 5 ms flush interval matches admission-design §4.3 ("Batched fsync
// every 5 ms"). rotationBytes is the file-size threshold; tests pass
// a smaller value to exercise rotation without writing a real GiB.
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

// Append serializes rec and writes it to the active file.
//   - Disputes (kinds 1,2): flush + Sync synchronously, return.
//   - All other kinds: buffer, return immediately.
//   - After every write, check size against rotationBytes; rotate if needed.
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

// rotateLocked renames the active file and opens a new one. Caller
// must hold w.mu.
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
```

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: all 6 new writer tests PASS, including the concurrent-append race-clean check.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/admission/tlog.go tracker/internal/admission/tlog_writer_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/admission): tlog writer — batched fsync + dispute sync + rotation

tlogWriter wraps the active admission.tlog file with three concerns:
  - per-Append routing by kind (disputes synchronous, others batched)
  - 5 ms flushLoop goroutine driving Sync() on the batched soft-state
  - size-triggered rotation at rotationBytes (production: 1 GiB)

Disputes have no ledger backing if lost (admission-design §4.3 "stricter
durability"), so they pay the per-write fsync cost. Settlements / transfers
/ starter_grants / heartbeat-rolls are recoverable from ledger replay,
so they ride the 5 ms batch.

Open-then-Append-to-existing test pins behavior on restart; concurrent-
append test exercises the mutex under -race.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: tlog reader for replay

**Files:**
- Modify: `tracker/internal/admission/tlog.go`
- Create: `tracker/internal/admission/tlog_reader_test.go`

`tlogReader` enumerates every record across rotated + active files in `seq` order, returning `(TLogRecord, error)`. On corrupted-tail (`ErrTLogTruncated` after a partial frame), the reader marks the file as having an uncommitted tail and stops at that point — the writer can later truncate to the last good byte. On `ErrTLogCorrupt` mid-file, the reader halts and surfaces an error so the operator can investigate (admission-design §7.2).

- [ ] **Step 1: Write the failing tests**

Create `tracker/internal/admission/tlog_reader_test.go`:

```go
package admission

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTLogReader_ReadsAllRecords(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.tlog")
	w, err := newTLogWriter(path, 5*time.Millisecond, 1<<30)
	require.NoError(t, err)

	for i := uint64(1); i <= 5; i++ {
		require.NoError(t, w.Append(TLogRecord{Seq: i, Ts: 100, Kind: TLogKindSettlement, Payload: []byte{byte(i)}}))
	}
	require.NoError(t, w.Close())

	got, lastGoodOffset, err := readTLogFile(path)
	require.NoError(t, err)
	assert.Len(t, got, 5)
	for i, rec := range got {
		assert.Equal(t, uint64(i+1), rec.Seq)
	}

	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, st.Size(), lastGoodOffset)
}

func TestTLogReader_TruncatedTail_HaltsAtLastGood(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.tlog")
	w, err := newTLogWriter(path, 5*time.Millisecond, 1<<30)
	require.NoError(t, err)
	for i := uint64(1); i <= 3; i++ {
		require.NoError(t, w.Append(TLogRecord{Seq: i, Ts: 100, Kind: TLogKindSettlement, Payload: []byte{byte(i)}}))
	}
	require.NoError(t, w.Close())

	// Append 5 garbage bytes so the next "frame" is truncated.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.Write([]byte{0xff, 0xff, 0xff, 0xff, 0xff})
	require.NoError(t, err)
	require.NoError(t, f.Close())

	got, lastGoodOffset, err := readTLogFile(path)
	require.NoError(t, err, "truncated tail is not an error — replay heals it")
	assert.Len(t, got, 3)
	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Less(t, lastGoodOffset, st.Size(), "lastGoodOffset must point before the truncated tail")
}

func TestTLogReader_MidFileCorruption_Errors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.tlog")
	w, err := newTLogWriter(path, 5*time.Millisecond, 1<<30)
	require.NoError(t, err)
	for i := uint64(1); i <= 3; i++ {
		require.NoError(t, w.Append(TLogRecord{Seq: i, Ts: 100, Kind: TLogKindSettlement, Payload: []byte{byte(i)}}))
	}
	require.NoError(t, w.Close())

	// Flip a byte inside the second record's CRC region.
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	require.NoError(t, err)
	const offsetIntoSecondRecord = 50 // arbitrary mid-frame
	_, err = f.WriteAt([]byte{0xab}, offsetIntoSecondRecord)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	got, _, err := readTLogFile(path)
	require.ErrorIs(t, err, ErrTLogCorrupt)
	// Pre-corruption record should be returned.
	assert.NotEmpty(t, got)
}

func TestEnumerateTLogFiles_OrdersBySeq(t *testing.T) {
	dir := t.TempDir()
	for _, suffix := range []string{".100", ".200", ".50", ""} {
		path := filepath.Join(dir, "admission.tlog"+suffix)
		require.NoError(t, os.WriteFile(path, []byte{}, 0o644))
	}

	files, err := enumerateTLogFiles(filepath.Join(dir, "admission.tlog"))
	require.NoError(t, err)
	require.Len(t, files, 4)
	assert.Equal(t, ".50", filepath.Ext(files[0]))
	assert.Equal(t, ".100", filepath.Ext(files[1]))
	assert.Equal(t, ".200", filepath.Ext(files[2]))
	assert.Equal(t, "admission.tlog", filepath.Base(files[3]), "active file last")
}
```

- [ ] **Step 2: Run, confirm FAIL**

Same `go test` command. Expected: `readTLogFile`, `enumerateTLogFiles` undefined.

- [ ] **Step 3: Append the reader**

Append to `tracker/internal/admission/tlog.go`:

```go
import (
	"sort"
	"strconv"
	"strings"
)
```

(Add to import block.)

```go
// readTLogFile reads every TLogRecord from the file at path. Returns the
// records in file order, the byte offset of the last successfully-parsed
// record's end (so callers can truncate any trailing garbage), and an error.
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
			// Trailing partial frame — heal silently.
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
```

Add `"path/filepath"` to imports if not already there.

- [ ] **Step 4: Run, confirm PASS**

Same command. All 4 reader tests + ordering test PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/admission/tlog.go tracker/internal/admission/tlog_reader_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/admission): tlog reader + file enumeration

readTLogFile parses one tlog file end-to-end with two distinct error
shapes:
  - ErrTLogTruncated at tail: silently healed (post-crash state).
    Caller takes lastGoodOffset as the true file end.
  - ErrTLogCorrupt mid-file: propagated to the operator (admission-
    design §7.2). Pre-corruption records are still returned so replay
    can apply them before halting.

enumerateTLogFiles returns rotated files in seq order followed by the
active file. Tests cover out-of-order naming on disk so the sort
matters.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: snapshot file format + write/read

**Files:**
- Create: `tracker/internal/admission/snapshot.go`
- Create: `tracker/internal/admission/snapshot_test.go`

Implements admission-design §4.3 snapshot format. `writeSnapshot(path, seq, ts, consumers, seeders)` writes atomically (`.tmp` + rename); `readSnapshot(path)` validates magic + format_version + trailer CRC and returns the populated maps.

This task lands the file format only. The 600-s emit goroutine + retention pruning lands in Task 6; load-into-Subsystem during replay lands in Task 7.

- [ ] **Step 1: Write the failing tests**

Create `tracker/internal/admission/snapshot_test.go`:

```go
package admission

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshot_WriteRead_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.snapshot.42")

	consumers := map[ids.IdentityID]*ConsumerCreditState{
		makeID(0xC1): {
			FirstSeenAt:     time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC),
			LastBalanceSeen: 1000,
		},
	}
	consumers[makeID(0xC1)].SettlementBuckets[0] = DayBucket{Total: 5, A: 5, DayStamp: time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)}

	seeders := map[ids.IdentityID]*SeederHeartbeatState{
		makeID(0x5E): {
			LastHeadroomEstimate: 7000,
			LastHeadroomSource:   2,
			LastHeadroomTs:       time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC),
			CanProbeUsage:        true,
		},
	}

	require.NoError(t, writeSnapshot(path, 42, time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC), consumers, seeders))

	snap, err := readSnapshot(path)
	require.NoError(t, err)
	assert.Equal(t, uint64(42), snap.Seq)
	require.Len(t, snap.Consumers, 1)
	require.Len(t, snap.Seeders, 1)

	cs := snap.Consumers[makeID(0xC1)]
	require.NotNil(t, cs)
	assert.Equal(t, int64(1000), cs.LastBalanceSeen)
	assert.Equal(t, uint32(5), cs.SettlementBuckets[0].Total)

	ss := snap.Seeders[makeID(0x5E)]
	require.NotNil(t, ss)
	assert.Equal(t, uint32(7000), ss.LastHeadroomEstimate)
	assert.True(t, ss.CanProbeUsage)
}

func TestSnapshot_AtomicWrite_NoPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.snapshot.1")
	require.NoError(t, writeSnapshot(path, 1, time.Now(), nil, nil))
	_, err := os.Stat(path + ".tmp")
	assert.True(t, os.IsNotExist(err), "tmp file should not survive successful rename")
}

func TestSnapshot_BadMagic_Rejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.snapshot.1")
	require.NoError(t, os.WriteFile(path, []byte{0xff, 0xff, 0xff, 0xff, 1, 0, 0, 0}, 0o644))

	_, err := readSnapshot(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "magic")
}

func TestSnapshot_BadFormatVersion_Rejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.snapshot.1")
	// Magic correct, format_version = 99
	buf := make([]byte, 24)
	binary.LittleEndian.PutUint32(buf[0:4], snapshotMagic)
	binary.LittleEndian.PutUint32(buf[4:8], 99)
	require.NoError(t, os.WriteFile(path, buf, 0o644))

	_, err := readSnapshot(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "format_version")
}

func TestSnapshot_TamperedTrailer_Rejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "admission.snapshot.1")
	require.NoError(t, writeSnapshot(path, 1, time.Now(), nil, nil))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	data[len(data)-1] ^= 0xff
	require.NoError(t, os.WriteFile(path, data, 0o644))

	_, err = readSnapshot(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trailer")
}

func TestEnumerateSnapshots_OrdersBySeq(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "admission.snapshot")
	for _, seq := range []uint64{50, 100, 75, 200} {
		require.NoError(t, writeSnapshot(fmt.Sprintf("%s.%d", prefix, seq), seq, time.Now(), nil, nil))
	}
	files, err := enumerateSnapshots(prefix)
	require.NoError(t, err)
	require.Len(t, files, 4)
	assert.Equal(t, uint64(50), files[0].seq)
	assert.Equal(t, uint64(75), files[1].seq)
	assert.Equal(t, uint64(100), files[2].seq)
	assert.Equal(t, uint64(200), files[3].seq)
}
```

Add imports `encoding/binary`, `fmt`, `github.com/token-bay/token-bay/shared/ids` to the test file.

- [ ] **Step 2: Run, confirm FAIL**

Same command. Expected: `writeSnapshot`, `readSnapshot`, `enumerateSnapshots`, `Snapshot` struct, `snapshotMagic` undefined.

- [ ] **Step 3: Write `snapshot.go`**

Create `tracker/internal/admission/snapshot.go`:

```go
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

// snapshotMagic identifies admission snapshot files. Spec §4.3: 0xADMSNAP1.
const snapshotMagic uint32 = 0xADMSNAP1

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

	// Consumers.
	count := make([]byte, 4)
	binary.LittleEndian.PutUint32(count, uint32(len(consumers))) //nolint:gosec // G115 — bounded by region size
	buf = append(buf, count...)
	for id, st := range consumers {
		buf = append(buf, encodeConsumerState(id, st)...)
	}

	// Seeders.
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

// readSnapshot validates magic + format_version + trailer CRC and
// returns the populated Snapshot. Failures return an error that
// callers (StartupReplay) use to fall back to the next-older snapshot.
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
```

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: all 6 new snapshot tests PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/admission/snapshot.go tracker/internal/admission/snapshot_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/admission): snapshot file format + atomic write/read

Per admission-design §4.3:
  magic(4) | format_version(4) | seq(8) | ts(8) |
  consumers_count(4) | repeated ConsumerState |
  seeders_count(4) | repeated SeederState |
  trailer_crc32(4)

Atomic write via <path>.tmp + fsync + rename. Read validates magic,
format_version, and trailer CRC; any failure surfaces an error so
StartupReplay can fall back to the next-older snapshot.

ConsumerState encodes FirstSeenAt + LastBalanceSeen + 30 day-buckets
each for {settlement, dispute, flow}. SeederState encodes 10
MinuteBuckets + heartbeat metadata.

The 600s emit goroutine + retention pruning lands in the next task;
load-into-Subsystem during replay lands in Task 7.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: snapshot emitter — periodic write + retention pruning

**Goal.** Spawn a ticker-driven goroutine that calls `writeSnapshot` every `cfg.SnapshotIntervalS` seconds, prunes older snapshots beyond `cfg.SnapshotsRetained`, and appends a `TLogKindSnapshotMark` record so replay can locate the snapshot's tlog seq.

- [ ] **Step 1: Failing test — `tracker/internal/admission/snapshot_emitter_test.go` (new)**

```go
package admission

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshotEmitter_TickEmitsAndPrunes(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	clk := newFixedClock(now)
	dir := t.TempDir()
	s, _ := openTempSubsystem(t,
		WithClock(clk.Now),
		WithSnapshotPrefix(filepath.Join(dir, "snap")),
		WithTLogPath(filepath.Join(dir, "admission.tlog")),
		WithSnapshotsRetained(2),
	)

	// Drive three emits manually (the ticker is wired separately).
	for i := 0; i < 3; i++ {
		require.NoError(t, s.runSnapshotEmitOnce(clk.Advance(0)))
	}

	files, err := enumerateSnapshots(filepath.Join(dir, "snap"))
	require.NoError(t, err)
	assert.Len(t, files, 2, "retention=2 keeps the two newest")
	assert.Greater(t, files[1].seq, files[0].seq)
}

func TestSnapshotEmitter_TickAppendsSnapshotMark(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	clk := newFixedClock(now)
	dir := t.TempDir()
	tlogPath := filepath.Join(dir, "admission.tlog")
	s, _ := openTempSubsystem(t,
		WithClock(clk.Now),
		WithSnapshotPrefix(filepath.Join(dir, "snap")),
		WithTLogPath(tlogPath),
	)

	require.NoError(t, s.runSnapshotEmitOnce(now))
	require.NoError(t, s.tlog.Close())

	recs, err := readTLogFile(tlogPath)
	require.NoError(t, err)
	require.NotEmpty(t, recs)
	last := recs[len(recs)-1]
	assert.Equal(t, TLogKindSnapshotMark, last.Kind)
}

func TestSnapshotEmitter_RaceCleanConcurrentEmitAndEvent(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	clk := newFixedClock(now)
	dir := t.TempDir()
	s, _ := openTempSubsystem(t,
		WithClock(clk.Now),
		WithSnapshotPrefix(filepath.Join(dir, "snap")),
		WithTLogPath(filepath.Join(dir, "admission.tlog")),
	)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			s.OnLedgerEvent(LedgerEvent{
				Kind: LedgerEventSettlement, ConsumerID: makeIDi(i),
				CostCredits: 10, Timestamp: now,
			})
		}
	}()
	for i := 0; i < 5; i++ {
		require.NoError(t, s.runSnapshotEmitOnce(now))
	}
	<-done
}
```

- [ ] **Step 2: Run, confirm RED** (`runSnapshotEmitOnce` and `WithSnapshotPrefix`/`WithTLogPath`/`WithSnapshotsRetained` don't exist yet).

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 -run TestSnapshotEmitter ./internal/admission/...
```

Expected: `undefined: WithSnapshotPrefix` etc. — compile error → RED.

- [ ] **Step 3: Implement — extend `tracker/internal/admission/snapshot.go`**

Add the emitter logic:

```go
// startSnapshotEmitter spawns the ticker goroutine. Called from Open after
// the aggregator. Tear-down is signaled by s.stop and waited via s.wg.
func (s *Subsystem) startSnapshotEmitter() {
	if s.cfg.SnapshotIntervalS == 0 {
		return // disabled
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(time.Duration(s.cfg.SnapshotIntervalS) * time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case ts := <-t.C:
				if err := s.runSnapshotEmitOnce(ts); err != nil {
					// Best-effort: emitter failure is observable via the
					// admission_snapshot_emit_failures_total metric (Task 12)
					// and the next tick will retry.
					continue
				}
			}
		}
	}()
}

// runSnapshotEmitOnce performs one write+prune+mark cycle. Exposed for tests
// so we don't have to wait for a real ticker.
func (s *Subsystem) runSnapshotEmitOnce(ts time.Time) error {
	if s.snapshotPrefix == "" {
		return nil // tests without persistence skip
	}
	seq := s.tlog.LastSeq() // monotone within process; replay sees it
	snap := s.snapshotState(seq, ts)
	path := fmt.Sprintf("%s.%020d", s.snapshotPrefix, seq)
	if err := writeSnapshot(path, snap); err != nil {
		return err
	}
	if err := s.pruneSnapshots(); err != nil {
		return err
	}
	if s.tlog != nil {
		mark := SnapshotMarkPayload{Seq: seq, Path: path}
		body, _ := mark.MarshalBinary()
		_ = s.tlog.Append(TLogRecord{Kind: TLogKindSnapshotMark, Payload: body, Ts: ts})
	}
	return nil
}

// snapshotState assembles a Snapshot from the live shard maps. Acquires each
// shard's read lock once; entries are deep-copied so subsequent mutation
// can't tear the snapshot.
func (s *Subsystem) snapshotState(seq uint64, ts time.Time) *Snapshot {
	snap := &Snapshot{
		Seq:       seq,
		Ts:        ts.UTC(),
		Consumers: make(map[ids.IdentityID]*ConsumerCreditState),
		Seeders:   make(map[ids.IdentityID]*SeederHeartbeatState),
	}
	for _, sh := range s.consumerShards {
		sh.mu.RLock()
		for id, st := range sh.m {
			cp := *st
			snap.Consumers[id] = &cp
		}
		sh.mu.RUnlock()
	}
	for _, sh := range s.seederShards {
		sh.mu.RLock()
		for id, st := range sh.m {
			cp := *st
			snap.Seeders[id] = &cp
		}
		sh.mu.RUnlock()
	}
	return snap
}

// pruneSnapshots removes the oldest snapshots so at most cfg.SnapshotsRetained
// remain on disk.
func (s *Subsystem) pruneSnapshots() error {
	files, err := enumerateSnapshots(s.snapshotPrefix)
	if err != nil {
		return err
	}
	keep := s.cfg.SnapshotsRetained
	if keep <= 0 {
		keep = 3 // sane default if config left it zero
	}
	if len(files) <= keep {
		return nil
	}
	for _, f := range files[:len(files)-keep] {
		_ = os.Remove(f.path)
	}
	return nil
}
```

Add the option helpers in `tracker/internal/admission/admission.go`:

```go
// WithSnapshotPrefix sets the snapshot path prefix (snapshots are written
// to <prefix>.<seq>). Empty disables persistence.
func WithSnapshotPrefix(prefix string) Option {
	return func(s *Subsystem) { s.snapshotPrefix = prefix }
}

// WithTLogPath sets the active tlog file path. Empty disables tlog writes.
func WithTLogPath(path string) Option {
	return func(s *Subsystem) { s.tlogPath = path }
}

// WithSnapshotsRetained overrides cfg.SnapshotsRetained for tests.
func WithSnapshotsRetained(n int) Option {
	return func(s *Subsystem) { s.cfg.SnapshotsRetained = n }
}
```

Extend `Subsystem` with the new fields and wire `Open` to construct the tlog writer + spawn the emitter:

```go
type Subsystem struct {
	// ... existing fields ...
	tlog           *tlogWriter
	tlogPath       string
	snapshotPrefix string
}

// In Open, after s.startAggregator():
if s.tlogPath != "" {
	w, err := openTLogWriter(s.tlogPath, s.cfg)
	if err != nil {
		return nil, fmt.Errorf("admission: open tlog: %w", err)
	}
	s.tlog = w
}
s.startSnapshotEmitter()
```

Update `helpers_test.go` — `openTempSubsystem` should default `tlogPath`/`snapshotPrefix` to a `t.TempDir()` if a test passes neither, so existing plan-2 tests don't suddenly need persistence.

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: all 3 new tests + every existing plan-2 test PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/admission/snapshot.go \
        tracker/internal/admission/snapshot_emitter_test.go \
        tracker/internal/admission/admission.go \
        tracker/internal/admission/helpers_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/admission): periodic snapshot emitter + retention pruning

Per admission-design §4.3 + §6.5:
  - startSnapshotEmitter spawns a SnapshotIntervalS ticker goroutine
  - each tick: writeSnapshot at current tlog.LastSeq(), prune to
    SnapshotsRetained, append TLogKindSnapshotMark
  - state is captured via snapshotState (deep-copy per shard under RLock)
    so live mutation never tears a write

WithSnapshotPrefix / WithTLogPath / WithSnapshotsRetained Options let
tests drive the cycle without real timers; runSnapshotEmitOnce is the
test seam.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: StartupReplay — snapshot load + tlog replay + ledger cross-check

**Goal.** Implement `(s *Subsystem) StartupReplay(ctx) error` per admission-design §5.7. The decision tree (per §3.4 of this plan):
1. Find newest snapshot. Try to load it. On failure, walk back to the next-older.
2. If all snapshots are corrupt or absent → set degraded mode, accept tlog/ledger from seq=0.
3. Replay tlog records with `seq > snapshot.seq`. A mid-file CRC error halts replay and surfaces.
4. Cross-check against the injected `LedgerSource` (a thin interface returning entries with seq > local-max). Apply any missing records.

- [ ] **Step 1: Failing tests — `tracker/internal/admission/replay_test.go` (new)**

```go
package admission

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplay_CleanRestart(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlog := filepath.Join(dir, "admission.tlog")
	prefix := filepath.Join(dir, "snap")

	// First boot: drive 5 settlements, emit snapshot, close.
	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlog), WithSnapshotPrefix(prefix))
	for i := 0; i < 5; i++ {
		s1.OnLedgerEvent(LedgerEvent{
			Kind: LedgerEventSettlement, ConsumerID: makeIDi(i),
			CostCredits: 10, Timestamp: now,
		})
	}
	require.NoError(t, s1.runSnapshotEmitOnce(now))
	require.NoError(t, s1.Close())

	// Second boot: replay → state matches.
	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlog), WithSnapshotPrefix(prefix), WithSkipAutoReplay())
	require.NoError(t, s2.StartupReplay(context.Background()))

	for i := 0; i < 5; i++ {
		shard := consumerShardFor(s2.consumerShards, makeIDi(i))
		st, ok := shard.get(makeIDi(i))
		require.True(t, ok, "consumer %d state not replayed", i)
		assert.Equal(t, uint32(1), st.SettlementBuckets[dayBucketIndex(now, rollingWindowDays)].Total)
	}
	assert.False(t, s2.DegradedMode())
}

func TestReplay_NoSnapshotOnlyTLog(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlog := filepath.Join(dir, "admission.tlog")

	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlog))
	s1.OnLedgerEvent(LedgerEvent{
		Kind: LedgerEventSettlement, ConsumerID: makeIDi(7),
		CostCredits: 10, Timestamp: now,
	})
	require.NoError(t, s1.Close())

	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlog), WithSkipAutoReplay())
	require.NoError(t, s2.StartupReplay(context.Background()))

	shard := consumerShardFor(s2.consumerShards, makeIDi(7))
	_, ok := shard.get(makeIDi(7))
	assert.True(t, ok)
	assert.False(t, s2.DegradedMode())
}

func TestReplay_CorruptLatestSnapshotFallsBackToOlder(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlog := filepath.Join(dir, "admission.tlog")
	prefix := filepath.Join(dir, "snap")

	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlog), WithSnapshotPrefix(prefix))
	s1.OnLedgerEvent(LedgerEvent{
		Kind: LedgerEventSettlement, ConsumerID: makeIDi(1),
		CostCredits: 10, Timestamp: now,
	})
	require.NoError(t, s1.runSnapshotEmitOnce(now)) // older snapshot
	s1.OnLedgerEvent(LedgerEvent{
		Kind: LedgerEventSettlement, ConsumerID: makeIDi(2),
		CostCredits: 10, Timestamp: now,
	})
	require.NoError(t, s1.runSnapshotEmitOnce(now)) // newer snapshot
	require.NoError(t, s1.Close())

	// Corrupt the newest snapshot (flip a body byte).
	files, err := enumerateSnapshots(prefix)
	require.NoError(t, err)
	require.Len(t, files, 2)
	corruptByteAt(t, files[1].path, 30)

	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlog), WithSnapshotPrefix(prefix), WithSkipAutoReplay())
	require.NoError(t, s2.StartupReplay(context.Background()))
	assert.False(t, s2.DegradedMode(), "older snapshot loaded successfully")
}

func TestReplay_AllSnapshotsCorrupt_DegradedMode(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlog := filepath.Join(dir, "admission.tlog")
	prefix := filepath.Join(dir, "snap")

	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlog), WithSnapshotPrefix(prefix))
	require.NoError(t, s1.runSnapshotEmitOnce(now))
	require.NoError(t, s1.runSnapshotEmitOnce(now))
	require.NoError(t, s1.Close())

	files, err := enumerateSnapshots(prefix)
	require.NoError(t, err)
	for _, f := range files {
		corruptByteAt(t, f.path, 30)
	}

	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlog), WithSnapshotPrefix(prefix), WithSkipAutoReplay())
	require.NoError(t, s2.StartupReplay(context.Background()))
	assert.True(t, s2.DegradedMode(), "all snapshots corrupt → degraded")
}

func TestReplay_PartialTrailingTLogFrameHeals(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlog := filepath.Join(dir, "admission.tlog")

	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlog))
	for i := 0; i < 3; i++ {
		s1.OnLedgerEvent(LedgerEvent{
			Kind: LedgerEventSettlement, ConsumerID: makeIDi(i),
			CostCredits: 10, Timestamp: now,
		})
	}
	require.NoError(t, s1.Close())
	truncateLastBytes(t, tlog, 2) // half-written tail

	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlog), WithSkipAutoReplay())
	require.NoError(t, s2.StartupReplay(context.Background()))
	// All 3 records were already flushed before the synthetic truncate.
	for i := 0; i < 3; i++ {
		_, ok := consumerShardFor(s2.consumerShards, makeIDi(i)).get(makeIDi(i))
		assert.True(t, ok)
	}
}

func TestReplay_MidFileCorruptHalts(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlog := filepath.Join(dir, "admission.tlog")

	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlog))
	for i := 0; i < 5; i++ {
		s1.OnLedgerEvent(LedgerEvent{
			Kind: LedgerEventSettlement, ConsumerID: makeIDi(i),
			CostCredits: 10, Timestamp: now,
		})
	}
	require.NoError(t, s1.Close())
	corruptByteAt(t, tlog, 20) // first record's body

	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlog), WithSkipAutoReplay())
	err := s2.StartupReplay(context.Background())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrTLogCorrupt)
}

func TestReplay_LedgerCrossCheck_AppliesMissingEntries(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlog := filepath.Join(dir, "admission.tlog")

	src := &fakeLedgerSource{events: []LedgerEvent{
		{Kind: LedgerEventSettlement, ConsumerID: makeIDi(99), CostCredits: 50, Timestamp: now},
	}}
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlog), WithSkipAutoReplay(), WithLedgerSource(src))
	require.NoError(t, s.StartupReplay(context.Background()))

	_, ok := consumerShardFor(s.consumerShards, makeIDi(99)).get(makeIDi(99))
	assert.True(t, ok, "ledger cross-check applied missing settlement")
}
```

`helpers_test.go` additions: `corruptByteAt`, `truncateLastBytes`, and `fakeLedgerSource` go alongside the existing fixtures.

- [ ] **Step 2: Run, confirm RED**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 -run TestReplay ./internal/admission/...
```

Expected: undefined symbols → RED.

- [ ] **Step 3: Implement — `tracker/internal/admission/replay.go` (new)**

```go
package admission

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// LedgerSource is the cross-check interface admission consults during
// StartupReplay. Real ledger emission is a separate plan; tests inject a
// fake.
type LedgerSource interface {
	// EventsAfter returns ledger events with seq > minSeq. May return zero
	// entries; must never return an unbounded sequence (caller iterates
	// once).
	EventsAfter(ctx context.Context, minSeq uint64) ([]LedgerEvent, error)
}

// nullLedgerSource is the default — no cross-check.
type nullLedgerSource struct{}

func (nullLedgerSource) EventsAfter(context.Context, uint64) ([]LedgerEvent, error) {
	return nil, nil
}

// WithLedgerSource injects a LedgerSource (production wiring lands in the
// ledger event-bus plan).
func WithLedgerSource(src LedgerSource) Option {
	return func(s *Subsystem) { s.ledgerSrc = src }
}

// WithSkipAutoReplay leaves startup replay to the caller's explicit
// StartupReplay invocation (production wiring will call it from cmd
// before serving requests).
func WithSkipAutoReplay() Option {
	return func(s *Subsystem) { s.skipAutoReplay = true }
}

// DegradedMode reports whether admission is running with no recoverable
// snapshot. While true, decisions still flow but persistence-derived
// features (recompute, /admission/queue replay) are suppressed.
func (s *Subsystem) DegradedMode() bool {
	return s.degradedMode.Load() != 0
}

// StartupReplay executes the §5.7 replay decision tree. Idempotent — safe
// to call once at boot. Caller is expected to acquire the subsystem before
// any external callers issue Decide.
func (s *Subsystem) StartupReplay(ctx context.Context) error {
	snap, err := s.loadNewestUsableSnapshot()
	switch {
	case err == nil && snap != nil:
		s.applySnapshot(snap)
	case errors.Is(err, errNoUsableSnapshot):
		s.degradedMode.Store(1)
	case err != nil:
		return err
	}

	startSeq := uint64(0)
	if snap != nil {
		startSeq = snap.Seq
	}
	if err := s.replayTLog(startSeq); err != nil {
		return err
	}

	src := s.ledgerSrc
	if src == nil {
		src = nullLedgerSource{}
	}
	missing, err := src.EventsAfter(ctx, s.tlog.LastSeq())
	if err != nil {
		return fmt.Errorf("admission/replay: ledger cross-check: %w", err)
	}
	for _, ev := range missing {
		s.OnLedgerEvent(ev)
	}
	return nil
}

var errNoUsableSnapshot = errors.New("admission: no usable snapshot")

func (s *Subsystem) loadNewestUsableSnapshot() (*Snapshot, error) {
	if s.snapshotPrefix == "" {
		return nil, errNoUsableSnapshot
	}
	files, err := enumerateSnapshots(s.snapshotPrefix)
	if err != nil {
		return nil, err
	}
	for i := len(files) - 1; i >= 0; i-- {
		snap, err := readSnapshot(files[i].path)
		if err == nil {
			return snap, nil
		}
		// On corruption, advance the snapshot_load_failures{which="snapshot"}
		// counter (Task 12 wires the gauge).
	}
	return nil, errNoUsableSnapshot
}

// applySnapshot installs every consumer + seeder state into the live
// shard maps. Caller holds no locks; getOrInit acquires per-shard.
func (s *Subsystem) applySnapshot(snap *Snapshot) {
	now := time.Now() // only used for FirstSeenAt fallback; snap carries its own
	for id, st := range snap.Consumers {
		live := consumerShardFor(s.consumerShards, id).getOrInit(id, now)
		*live = *st
	}
	for id, st := range snap.Seeders {
		live := seederShardFor(s.seederShards, id).getOrInit(id, now)
		*live = *st
	}
}

// replayTLog reads every record with seq > startSeq and re-applies it. Mid-file
// CRC corruption halts and surfaces ErrTLogCorrupt; trailing-frame truncation
// is healed by the reader.
func (s *Subsystem) replayTLog(startSeq uint64) error {
	if s.tlogPath == "" {
		return nil
	}
	files, err := enumerateTLogFiles(s.tlogPath)
	if err != nil {
		return err
	}
	for _, f := range files {
		recs, err := readTLogFile(f)
		if err != nil {
			return err
		}
		for _, r := range recs {
			if r.Seq <= startSeq {
				continue
			}
			s.applyTLogRecord(r)
		}
	}
	return nil
}

// applyTLogRecord routes a replayed record to the same in-memory mutator
// OnLedgerEvent uses, but skips the tlog write (would cause replay loops).
func (s *Subsystem) applyTLogRecord(r TLogRecord) {
	switch r.Kind {
	case TLogKindSettlement:
		var p SettlementPayload
		if err := p.UnmarshalBinary(r.Payload); err == nil {
			s.OnLedgerEvent(p.toLedgerEvent(r.Ts))
		}
	case TLogKindDispute:
		var p DisputePayload
		if err := p.UnmarshalBinary(r.Payload); err == nil {
			s.OnLedgerEvent(p.toLedgerEvent(r.Ts))
		}
	case TLogKindTransfer:
		var p TransferPayload
		if err := p.UnmarshalBinary(r.Payload); err == nil {
			s.OnLedgerEvent(p.toLedgerEvent(r.Ts))
		}
	case TLogKindStarterGrant:
		var p StarterGrantPayload
		if err := p.UnmarshalBinary(r.Payload); err == nil {
			s.OnLedgerEvent(p.toLedgerEvent(r.Ts))
		}
	case TLogKindSnapshotMark, TLogKindOperatorOverride:
		// Markers and audit-only — no in-memory mutation.
	}
}
```

Add to `Subsystem` (in `admission.go`):

```go
type Subsystem struct {
	// ... existing ...
	ledgerSrc      LedgerSource
	skipAutoReplay bool
	degradedMode   atomic.Uint32
}
```

Modify `Open` to call `StartupReplay` automatically unless `skipAutoReplay` is set:

```go
if !s.skipAutoReplay {
	if err := s.StartupReplay(context.Background()); err != nil {
		_ = s.Close()
		return nil, err
	}
}
```

Note: `applyTLogRecord` calls back into `OnLedgerEvent`, which will append a fresh tlog record. To avoid replay-loop writes, gate the append in Task 8's `OnLedgerEvent` rewrite with an `s.replaying atomic.Bool`. Set it true around `replayTLog`.

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: every replay test PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/admission/replay.go \
        tracker/internal/admission/replay_test.go \
        tracker/internal/admission/admission.go \
        tracker/internal/admission/helpers_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/admission): StartupReplay — snapshot + tlog + ledger cross-check

Per admission-design §5.7:
  - Walk newest→oldest snapshots; first one that loads is applied.
  - All-corrupt → degraded mode (decisions still flow, persistence
    features suppressed).
  - Replay tlog records with seq > snapshot.seq; mid-file CRC halts,
    trailing-frame truncation heals.
  - LedgerSource cross-check fills any gap between local tlog and
    authoritative ledger. v1 default is null; production wiring lands
    in the ledger event-bus plan.

WithLedgerSource / WithSkipAutoReplay Options + DegradedMode accessor.
applyTLogRecord routes by Kind back through OnLedgerEvent (Task 8 will
gate the tlog append on s.replaying so replay doesn't double-write).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: Wire OnLedgerEvent → tlog write

**Goal.** Modify the existing `OnLedgerEvent` so each in-memory state change is persisted to the tlog *first*, then applied. A write failure aborts the apply (state never gets ahead of disk). Disputes get synchronous fsync; everything else is batched. During replay, tlog writes are suppressed via the `s.replaying` flag.

- [ ] **Step 1: Failing test additions — `tracker/internal/admission/events_test.go` (extend)**

Add at the end of the existing file:

```go
func TestOnLedgerEvent_PersistsSettlementToTLog(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlogPath := filepath.Join(dir, "admission.tlog")
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlogPath))

	s.OnLedgerEvent(LedgerEvent{
		Kind: LedgerEventSettlement, ConsumerID: makeIDi(11),
		SeederID: makeIDi(99), CostCredits: 42, Timestamp: now,
	})
	require.NoError(t, s.tlog.Close())

	recs, err := readTLogFile(tlogPath)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, TLogKindSettlement, recs[0].Kind)

	var p SettlementPayload
	require.NoError(t, p.UnmarshalBinary(recs[0].Payload))
	assert.Equal(t, makeIDi(11), p.ConsumerID)
	assert.Equal(t, uint64(42), p.CostCredits)
}

func TestOnLedgerEvent_DisputeForcesFsync(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlogPath := filepath.Join(dir, "admission.tlog")
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlogPath))

	s.OnLedgerEvent(LedgerEvent{
		Kind: LedgerEventDisputeFiled, ConsumerID: makeIDi(11),
		Timestamp: now,
	})
	// Dispute path syncs synchronously — readable without explicit Close.
	recs, err := readTLogFile(tlogPath)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, TLogKindDispute, recs[0].Kind)
}

func TestOnLedgerEvent_TransferRoundtrip(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlogPath := filepath.Join(dir, "admission.tlog")
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlogPath))

	s.OnLedgerEvent(LedgerEvent{
		Kind: LedgerEventTransferIn, ConsumerID: makeIDi(11),
		CostCredits: 100, Timestamp: now,
	})
	require.NoError(t, s.tlog.Close())

	recs, err := readTLogFile(tlogPath)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, TLogKindTransfer, recs[0].Kind)
}

func TestOnLedgerEvent_StarterGrantRoundtrip(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlogPath := filepath.Join(dir, "admission.tlog")
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlogPath))

	s.OnLedgerEvent(LedgerEvent{
		Kind: LedgerEventStarterGrant, ConsumerID: makeIDi(11),
		CostCredits: 1000, Timestamp: now,
	})
	require.NoError(t, s.tlog.Close())

	recs, err := readTLogFile(tlogPath)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, TLogKindStarterGrant, recs[0].Kind)
}

func TestOnLedgerEvent_DuringReplay_NoDoubleWrite(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlogPath := filepath.Join(dir, "admission.tlog")
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlogPath))

	s.replaying.Store(true)
	s.OnLedgerEvent(LedgerEvent{
		Kind: LedgerEventSettlement, ConsumerID: makeIDi(7),
		CostCredits: 10, Timestamp: now,
	})
	s.replaying.Store(false)
	require.NoError(t, s.tlog.Close())

	recs, err := readTLogFile(tlogPath)
	require.NoError(t, err)
	assert.Empty(t, recs, "replay path must not append to tlog")
}
```

- [ ] **Step 2: Run, confirm RED.**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 -run TestOnLedgerEvent ./internal/admission/...
```

Expected: tlog write missing → settlement records empty → RED.

- [ ] **Step 3: Implement — modify `tracker/internal/admission/events.go`**

Add the `replaying` flag on `Subsystem` (`admission.go`):

```go
type Subsystem struct {
	// ... existing ...
	replaying atomic.Bool
}
```

Wire `replayTLog` (Task 7) to set `s.replaying` true on entry, false on exit.

Rewrite `OnLedgerEvent` so each branch builds + appends a tlog record before mutating:

```go
func (s *Subsystem) OnLedgerEvent(ev LedgerEvent) {
	switch ev.Kind {
	case LedgerEventSettlement:
		if err := s.persistEvent(TLogKindSettlement, settlementPayloadFor(ev), ev.Timestamp); err != nil {
			return
		}
		s.applySettlement(ev)
	case LedgerEventTransferIn:
		if err := s.persistEvent(TLogKindTransfer, transferPayloadFor(ev, true), ev.Timestamp); err != nil {
			return
		}
		s.applyTransfer(ev.ConsumerID, signedDelta(ev.CostCredits, true), ev.Timestamp)
	case LedgerEventTransferOut:
		if err := s.persistEvent(TLogKindTransfer, transferPayloadFor(ev, false), ev.Timestamp); err != nil {
			return
		}
		s.applyTransfer(ev.ConsumerID, signedDelta(ev.CostCredits, false), ev.Timestamp)
	case LedgerEventStarterGrant:
		if err := s.persistEvent(TLogKindStarterGrant, starterGrantPayloadFor(ev), ev.Timestamp); err != nil {
			return
		}
		s.applyStarterGrant(ev)
	case LedgerEventDisputeFiled:
		if err := s.persistEvent(TLogKindDispute, disputePayloadFor(ev, false), ev.Timestamp); err != nil {
			return
		}
		s.applyDispute(ev, false)
	case LedgerEventDisputeResolved:
		if !ev.DisputeUpheld {
			return
		}
		if err := s.persistEvent(TLogKindDispute, disputePayloadFor(ev, true), ev.Timestamp); err != nil {
			return
		}
		s.applyDispute(ev, true)
	case LedgerEventUnspecified:
		// no-op
	}
}

// persistEvent appends a TLogRecord. Skipped when s.replaying is true (the
// records being applied are coming from disk; double-writing would create
// an infinite-growth loop on every restart).
func (s *Subsystem) persistEvent(kind TLogKind, body interface{ MarshalBinary() ([]byte, error) }, ts time.Time) error {
	if s.replaying.Load() || s.tlog == nil {
		return nil
	}
	payload, err := body.MarshalBinary()
	if err != nil {
		return err
	}
	return s.tlog.Append(TLogRecord{Kind: kind, Payload: payload, Ts: ts})
}

func settlementPayloadFor(ev LedgerEvent) SettlementPayload { /* ... field copy ... */ }
func transferPayloadFor(ev LedgerEvent, in bool) TransferPayload { /* ... */ }
func starterGrantPayloadFor(ev LedgerEvent) StarterGrantPayload { /* ... */ }
func disputePayloadFor(ev LedgerEvent, upheld bool) DisputePayload { /* ... */ }
```

The four `*payloadFor` helpers map a `LedgerEvent` onto its payload struct (defined in Task 2). Each is ~5 lines of field assignment.

The reverse direction (`payload.toLedgerEvent`) used by `applyTLogRecord` (Task 7) lives alongside its payload type in `tracker/internal/admission/tlog_payloads.go`.

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: every events test (existing 7 + 5 new) PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/admission/events.go \
        tracker/internal/admission/events_test.go \
        tracker/internal/admission/admission.go \
        tracker/internal/admission/tlog_payloads.go
git commit -m "$(cat <<'EOF'
feat(tracker/admission): persist OnLedgerEvent to tlog before mutating

Each branch of OnLedgerEvent now:
  1. Marshals its kind-specific payload (Task 2 types).
  2. Appends a TLogRecord via s.tlog.Append.
  3. Applies the in-memory mutation only on append success.

Persist-then-apply ordering means a write failure leaves disk and memory
consistent: the event is dropped, the next OnLedgerEvent call is a fresh
attempt.

Disputes get synchronous fsync (kind-based routing in the tlogWriter from
Task 3); other kinds are batched.

s.replaying is set during StartupReplay so applyTLogRecord can re-route
through OnLedgerEvent without double-writing.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 9: Failure-mode integration + degraded-mode behavior

**Goal.** Pin the failure modes from admission-design §7.1-§7.4 with cross-cutting integration tests, and verify that degraded-mode admission still serves Decide while persistence is impaired.

DegradedMode() and the atomic flag already land in Task 7. Task 9 layers metrics, decision-mode behavior, and the §7.x scenario tests.

- [ ] **Step 1: Failing tests — `tracker/internal/admission/degraded_mode_test.go` (new)**

```go
package admission

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// §7.1 — Process crash between tlog flush and ledger commit.
// Plan: write 5 records, simulate crash by closing the writer mid-batch, then
// inject a ledger source that knows about a 6th. Replay should apply 1-5 from
// tlog and 6 from ledger.
func TestFailure_71_TLogGapFilledByLedgerCrossCheck(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlogPath := filepath.Join(dir, "admission.tlog")
	src := &fakeLedgerSource{}

	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlogPath))
	for i := 0; i < 5; i++ {
		ev := LedgerEvent{
			Kind: LedgerEventSettlement, ConsumerID: makeIDi(i),
			CostCredits: 10, Timestamp: now,
		}
		s1.OnLedgerEvent(ev)
		src.append(ev)
	}
	src.append(LedgerEvent{
		Kind: LedgerEventSettlement, ConsumerID: makeIDi(99), // missing from tlog
		CostCredits: 10, Timestamp: now,
	})
	require.NoError(t, s1.Close())

	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlogPath), WithSkipAutoReplay(), WithLedgerSource(src))
	require.NoError(t, s2.StartupReplay(context.Background()))

	for i := 0; i < 5; i++ {
		_, ok := consumerShardFor(s2.consumerShards, makeIDi(i)).get(makeIDi(i))
		assert.True(t, ok, "tlog-derived consumer %d", i)
	}
	_, ok := consumerShardFor(s2.consumerShards, makeIDi(99)).get(makeIDi(99))
	assert.True(t, ok, "ledger-cross-check filled the gap")
}

// §7.2 — tlog mid-record corruption surfaces; decisions still flow.
func TestFailure_72_MidTLogCorruptionDoesNotKillDecide(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlogPath := filepath.Join(dir, "admission.tlog")

	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlogPath))
	for i := 0; i < 3; i++ {
		s1.OnLedgerEvent(LedgerEvent{
			Kind: LedgerEventSettlement, ConsumerID: makeIDi(i),
			CostCredits: 10, Timestamp: now,
		})
	}
	require.NoError(t, s1.Close())
	corruptByteAt(t, tlogPath, 30) // mid-first-record body

	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(tlogPath), WithSkipAutoReplay())
	err := s2.StartupReplay(context.Background())
	require.Error(t, err)

	// Decide path remains usable even with halted replay.
	res := s2.Decide(makeID(0x77), nil, now)
	assert.Equal(t, OutcomeAdmit, res.Outcome, "boot-time admit until aggregator publishes")
}

// §7.3 — Snapshot corruption falls back to next-older. Already pinned in
// TestReplay_CorruptLatestSnapshotFallsBackToOlder; this test asserts that
// the failure increments the metric counter.
func TestFailure_73_SnapshotCorruptionIncrementsCounter(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	prefix := filepath.Join(dir, "snap")

	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithSnapshotPrefix(prefix),
		WithTLogPath(filepath.Join(dir, "admission.tlog")))
	require.NoError(t, s1.runSnapshotEmitOnce(now))
	require.NoError(t, s1.runSnapshotEmitOnce(now))
	require.NoError(t, s1.Close())

	files, err := enumerateSnapshots(prefix)
	require.NoError(t, err)
	corruptByteAt(t, files[1].path, 30)

	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithSnapshotPrefix(prefix),
		WithTLogPath(filepath.Join(dir, "admission.tlog")),
		WithSkipAutoReplay())
	require.NoError(t, s2.StartupReplay(context.Background()))
	assert.False(t, s2.DegradedMode())
	assert.GreaterOrEqual(t, s2.SnapshotLoadFailures(), uint64(1))
}

// §7.4 — All snapshots corrupt → DegradedMode + Decide still flows.
func TestFailure_74_DegradedModeKeepsDecideAvailable(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	prefix := filepath.Join(dir, "snap")
	tlogPath := filepath.Join(dir, "admission.tlog")

	s1, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithSnapshotPrefix(prefix), WithTLogPath(tlogPath))
	require.NoError(t, s1.runSnapshotEmitOnce(now))
	require.NoError(t, s1.runSnapshotEmitOnce(now))
	require.NoError(t, s1.Close())

	files, _ := enumerateSnapshots(prefix)
	for _, f := range files {
		corruptByteAt(t, f.path, 30)
	}

	s2, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithSnapshotPrefix(prefix), WithTLogPath(tlogPath),
		WithSkipAutoReplay())
	require.NoError(t, s2.StartupReplay(context.Background()))
	require.True(t, s2.DegradedMode())

	res := s2.Decide(makeID(0x55), nil, now)
	assert.Equal(t, OutcomeAdmit, res.Outcome,
		"degraded mode admits new arrivals — credit history is null until rebuilt")
}
```

- [ ] **Step 2: Run, confirm RED** (`SnapshotLoadFailures` does not exist yet, integration paths show wrong expected behavior).

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 -run TestFailure ./internal/admission/...
```

- [ ] **Step 3: Implement — `tracker/internal/admission/replay.go` (extend)**

Add the counter accessors used by the tests + Task 12 metrics:

```go
type Subsystem struct {
	// ... existing ...
	snapshotLoadFailures atomic.Uint64
	tlogCorruptions      atomic.Uint64
}

// SnapshotLoadFailures is read by the metrics Collector (Task 12).
func (s *Subsystem) SnapshotLoadFailures() uint64 { return s.snapshotLoadFailures.Load() }

// TLogCorruptions is read by the metrics Collector (Task 12).
func (s *Subsystem) TLogCorruptions() uint64 { return s.tlogCorruptions.Load() }
```

Update `loadNewestUsableSnapshot` to bump the counter on each failed read:

```go
for i := len(files) - 1; i >= 0; i-- {
	snap, err := readSnapshot(files[i].path)
	if err == nil {
		return snap, nil
	}
	s.snapshotLoadFailures.Add(1)
}
```

Update `replayTLog` to bump `tlogCorruptions` when `readTLogFile` returns `ErrTLogCorrupt` mid-file (the reader itself heals trailing truncation; mid-file is the only counted case).

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: §7.1-§7.4 tests PASS, every prior test still PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/admission/replay.go \
        tracker/internal/admission/degraded_mode_test.go
git commit -m "$(cat <<'EOF'
test(tracker/admission): §7.1-7.4 failure-mode + degraded-mode integration

Pins admission-design §7.1 (crash mid-event, ledger cross-check fills),
§7.2 (mid-tlog corruption, decide still flows), §7.3 (snapshot
corruption falls back), and §7.4 (all-corrupt → degraded, decide
admits new arrivals).

Adds SnapshotLoadFailures + TLogCorruptions accessors used by Task 12
metrics; failure counters bump on each unsuccessful load attempt.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 10: Admin handlers — 11 routes + RegisterMux + BasicAuthGuard

**Goal.** Provide an HTTP-handler surface for operator tooling per admission-design §9.1. Plan 3 ships handlers as `http.Handler` instances + a `BasicAuthGuard` middleware; mounting on a real listener with TLS belongs to the tracker control-plane plan.

Routes (all under `/admission/`):

| Method | Path | Auth | Body / response |
|---|---|---|---|
| GET | `/status` | yes | `{supply, queue_depth, pressure, thresholds}` |
| GET | `/queue` | yes | `[{consumer_id, score, enqueued_at, effective_priority}]` |
| GET | `/consumer/{id}` | yes | `{signals, score, recent_attestations}` |
| GET | `/seeder/{id}` | yes | `{heartbeat_state}` |
| POST | `/queue/drain` | yes | `{n}` → drained list (operator override) |
| POST | `/queue/eject/{request_id}` | yes | (operator override) |
| POST | `/snapshot` | yes | (operator override) — force `runSnapshotEmitOnce` |
| POST | `/recompute/{consumer_id}` | yes | (operator override) — re-derive from ledger |
| GET | `/peers/blocklist` | yes | `[peer_id]` |
| POST | `/peers/blocklist/{peer_id}` | yes | (operator override) |
| DELETE | `/peers/blocklist/{peer_id}` | yes | (operator override) |

- [ ] **Step 1: Failing tests — `tracker/internal/admission/admin_test.go` (new)**

```go
package admission

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newAdminHTTPServer(t *testing.T, s *Subsystem) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	guard := func(next http.Handler) http.Handler {
		return BasicAuthGuard(next, func(token string) bool { return token == "test-token" })
	}
	s.RegisterMux(mux, guard)
	return httptest.NewServer(mux)
}

func req(t *testing.T, method, url, token, body string) *http.Response {
	t.Helper()
	r, err := http.NewRequest(method, url, strings.NewReader(body))
	require.NoError(t, err)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(r)
	require.NoError(t, err)
	return resp
}

func TestAdmin_Status(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 0.5})

	srv := newAdminHTTPServer(t, s)
	defer srv.Close()

	resp := req(t, "GET", srv.URL+"/admission/status", "test-token", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.InDelta(t, 0.5, out["pressure"], 1e-9)
	assert.InDelta(t, 10.0, out["supply_total_headroom"], 1e-9)
}

func TestAdmin_Status_RejectsMissingAuth(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	srv := newAdminHTTPServer(t, s)
	defer srv.Close()

	resp := req(t, "GET", srv.URL+"/admission/status", "", "")
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAdmin_Queue_ListsEntries(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 1.0})
	for i := 0; i < 3; i++ {
		s.Decide(makeIDi(i), nil, now)
	}
	srv := newAdminHTTPServer(t, s)
	defer srv.Close()

	resp := req(t, "GET", srv.URL+"/admission/queue", "test-token", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out []map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Len(t, out, 3)
}

func TestAdmin_QueueDrain_WritesOverride(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlogPath := dir + "/admission.tlog"
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlogPath))
	s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: 10, Pressure: 1.0})
	for i := 0; i < 3; i++ {
		s.Decide(makeIDi(i), nil, now)
	}
	srv := newAdminHTTPServer(t, s)
	defer srv.Close()

	resp := req(t, "POST", srv.URL+"/admission/queue/drain", "test-token", `{"n":2}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, s.tlog.Close())

	recs, err := readTLogFile(tlogPath)
	require.NoError(t, err)
	var found bool
	for _, r := range recs {
		if r.Kind == TLogKindOperatorOverride {
			found = true
			break
		}
	}
	assert.True(t, found, "queue/drain emitted OPERATOR_OVERRIDE")
}

func TestAdmin_Consumer_404OnUnknown(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)))
	srv := newAdminHTTPServer(t, s)
	defer srv.Close()

	resp := req(t, "GET", srv.URL+"/admission/consumer/00000000000000000000000000000000000000000000000000000000000000ff",
		"test-token", "")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestAdmin_Snapshot_TriggersEmit(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(dir+"/admission.tlog"),
		WithSnapshotPrefix(dir+"/snap"))
	srv := newAdminHTTPServer(t, s)
	defer srv.Close()

	resp := req(t, "POST", srv.URL+"/admission/snapshot", "test-token", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	files, _ := enumerateSnapshots(dir + "/snap")
	assert.Len(t, files, 1)
}

func TestAdmin_PeersBlocklist_AddRemove(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)),
		WithTLogPath(t.TempDir()+"/admission.tlog"))
	srv := newAdminHTTPServer(t, s)
	defer srv.Close()

	peer := "00000000000000000000000000000000000000000000000000000000000000aa"

	resp := req(t, "POST", srv.URL+"/admission/peers/blocklist/"+peer, "test-token", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp = req(t, "GET", srv.URL+"/admission/peers/blocklist", "test-token", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out []string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	assert.Contains(t, out, peer)

	resp = req(t, "DELETE", srv.URL+"/admission/peers/blocklist/"+peer, "test-token", "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
}
```

- [ ] **Step 2: Run, confirm RED.**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 -run TestAdmin ./internal/admission/...
```

- [ ] **Step 3: Implement — `tracker/internal/admission/admin.go` (new)**

```go
package admission

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/token-bay/token-bay/shared/ids"
)

// MuxGuard wraps a route handler with auth + audit. Tests inject a guard
// that checks "test-token"; production wires a real bearer-token validator.
type MuxGuard func(http.Handler) http.Handler

// RegisterMux mounts the admission admin handlers under /admission/. All
// routes pass through guard. Plan 3 ships handlers as net/http; mounting
// on a real TLS listener belongs to the tracker control-plane plan.
func (s *Subsystem) RegisterMux(mux *http.ServeMux, guard MuxGuard) {
	if guard == nil {
		guard = func(h http.Handler) http.Handler { return h }
	}
	mux.Handle("GET /admission/status", guard(http.HandlerFunc(s.handleStatus)))
	mux.Handle("GET /admission/queue", guard(http.HandlerFunc(s.handleQueueList)))
	mux.Handle("GET /admission/consumer/{id}", guard(http.HandlerFunc(s.handleConsumer)))
	mux.Handle("GET /admission/seeder/{id}", guard(http.HandlerFunc(s.handleSeeder)))
	mux.Handle("POST /admission/queue/drain", guard(http.HandlerFunc(s.handleQueueDrain)))
	mux.Handle("POST /admission/queue/eject/{request_id}", guard(http.HandlerFunc(s.handleQueueEject)))
	mux.Handle("POST /admission/snapshot", guard(http.HandlerFunc(s.handleSnapshotForce)))
	mux.Handle("POST /admission/recompute/{consumer_id}", guard(http.HandlerFunc(s.handleRecompute)))
	mux.Handle("GET /admission/peers/blocklist", guard(http.HandlerFunc(s.handleBlocklistList)))
	mux.Handle("POST /admission/peers/blocklist/{peer_id}", guard(http.HandlerFunc(s.handleBlocklistAdd)))
	mux.Handle("DELETE /admission/peers/blocklist/{peer_id}", guard(http.HandlerFunc(s.handleBlocklistRemove)))
}

// BasicAuthGuard returns middleware that checks an "Authorization: Bearer X"
// header; validator decides whether X is a valid operator token.
func BasicAuthGuard(next http.Handler, validator func(token string) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		hdr := r.Header.Get("Authorization")
		if !strings.HasPrefix(hdr, prefix) || !validator(strings.TrimPrefix(hdr, prefix)) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="admission"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Subsystem) handleStatus(w http.ResponseWriter, _ *http.Request) {
	snap := s.Supply()
	out := map[string]any{
		"pressure":              snap.Pressure,
		"supply_total_headroom": snap.TotalHeadroom,
		"queue_depth":           s.queue.Len(),
		"thresholds": map[string]float64{
			"admit":  s.cfg.PressureAdmitThreshold,
			"reject": s.cfg.PressureRejectThreshold,
		},
	}
	writeJSON(w, http.StatusOK, out)
}

// ... handlers per route. Each mutating handler ends with
//     s.writeOperatorOverride(r.Context(), action, params).
// Each lookup handler decodes the path's hex-32 id with shared/ids,
// returns 404 on missing, 200 with the snapshot value otherwise.

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func parseID(s string) (ids.IdentityID, error) {
	return ids.ParseHex(s) // assumes shared/ids has ParseHex; if not, write the
	// 32-byte hex decode inline. (No new shared/ helper from plan 3.)
}
```

The remaining 10 handlers are short — typical shape:

```go
func (s *Subsystem) handleConsumer(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	st, ok := consumerShardFor(s.consumerShards, id).get(id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	signals := s.signalsFor(st, s.nowFn())
	score := s.compositeScore(signals)
	writeJSON(w, http.StatusOK, map[string]any{
		"signals": signals,
		"score":   score,
	})
}
```

Each mutating handler emits an operator override:

```go
func (s *Subsystem) handleQueueDrain(w http.ResponseWriter, r *http.Request) {
	var body struct{ N int `json:"n"` }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	drained := s.queue.DrainTop(body.N)
	if err := s.writeOperatorOverride(r.Context(), "queue/drain",
		mustJSON(map[string]any{"n": body.N})); err != nil {
		http.Error(w, "tlog write failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"drained": drained})
}
```

The peer blocklist is held on `s` as `blocklist map[ids.IdentityID]struct{}` guarded by `blocklistMu sync.RWMutex`. Add/remove handlers manipulate it; the GET handler returns the hex IDs sorted.

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

Expected: every admin test PASS, every prior test still PASS.

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/admission/admin.go \
        tracker/internal/admission/admin_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/admission): admin HTTP handlers + BasicAuthGuard

Per admission-design §9.1: 11 routes under /admission/, each gated by
a MuxGuard (BasicAuthGuard ships as the canonical middleware; tests
inject a fake validator).

Routes:
  GET  /status                         queue + supply snapshot
  GET  /queue                          ranked queue contents
  GET  /consumer/{id}                  signals + composite score
  GET  /seeder/{id}                    heartbeat + headroom
  POST /queue/drain                    body {n}, emits OPERATOR_OVERRIDE
  POST /queue/eject/{request_id}       emits OPERATOR_OVERRIDE
  POST /snapshot                       force runSnapshotEmitOnce
  POST /recompute/{consumer_id}        re-derive from ledger
  GET  /peers/blocklist                hex-encoded peer IDs
  POST /peers/blocklist/{peer_id}      emits OPERATOR_OVERRIDE
  DELETE /peers/blocklist/{peer_id}    emits OPERATOR_OVERRIDE

Mounting on a TLS listener with a real validator belongs to the
tracker control-plane plan. RegisterMux + BasicAuthGuard let any
caller wire admission into an existing http.ServeMux.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 11: Operator override TLogRecord helper

**Goal.** Centralize the operator-override audit trail in one helper so admin handlers (Task 10) emit consistent records and the test surface stays small.

- [ ] **Step 1: Failing test — `tracker/internal/admission/operator_override_test.go` (new)**

```go
package admission

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOperatorOverride_AppendsTLogRecord(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlogPath := filepath.Join(dir, "admission.tlog")
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlogPath))

	ctx := WithOperatorContext(context.Background(), "operator-7")
	require.NoError(t, s.writeOperatorOverride(ctx, "queue/drain", []byte(`{"n":2}`)))
	require.NoError(t, s.tlog.Close())

	recs, err := readTLogFile(tlogPath)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, TLogKindOperatorOverride, recs[0].Kind)

	var p OperatorOverridePayload
	require.NoError(t, p.UnmarshalBinary(recs[0].Payload))
	assert.Equal(t, "operator-7", p.OperatorID)
	assert.Equal(t, "queue/drain", p.Action)
	assert.JSONEq(t, `{"n":2}`, string(p.ParamsJSON))
}

func TestOperatorOverride_NoOpWhenTLogDisabled(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now))) // no WithTLogPath
	ctx := WithOperatorContext(context.Background(), "operator-7")
	assert.NoError(t, s.writeOperatorOverride(ctx, "queue/drain", []byte(`{}`)))
}

func TestOperatorOverride_AnonymousWhenContextMissing(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	tlogPath := filepath.Join(dir, "admission.tlog")
	s, _ := openTempSubsystem(t, WithClock(staticClockFn(now)), WithTLogPath(tlogPath))
	require.NoError(t, s.writeOperatorOverride(context.Background(), "snapshot", []byte("{}")))
	require.NoError(t, s.tlog.Close())

	recs, err := readTLogFile(tlogPath)
	require.NoError(t, err)
	require.Len(t, recs, 1)
	var p OperatorOverridePayload
	require.NoError(t, p.UnmarshalBinary(recs[0].Payload))
	assert.Equal(t, "anonymous", p.OperatorID)
}
```

- [ ] **Step 2: Run, confirm RED.**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 -run TestOperatorOverride ./internal/admission/...
```

- [ ] **Step 3: Implement — `tracker/internal/admission/operator_override.go` (new)**

```go
package admission

import (
	"context"
	"time"
)

type ctxKey int

const operatorIDKey ctxKey = 1

// WithOperatorContext stamps the request's operator identity onto ctx.
// Callers (admin handlers + tests) chain this when constructing a request
// context that should carry an audit trail.
func WithOperatorContext(ctx context.Context, operatorID string) context.Context {
	return context.WithValue(ctx, operatorIDKey, operatorID)
}

// operatorIDFromContext returns the bound operator id or "anonymous" when
// the key is missing.
func operatorIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(operatorIDKey).(string); ok && v != "" {
		return v
	}
	return "anonymous"
}

// writeOperatorOverride appends an OPERATOR_OVERRIDE record. Returns nil
// (and is a no-op) when the tlog is disabled.
func (s *Subsystem) writeOperatorOverride(ctx context.Context, action string, paramsJSON []byte) error {
	if s.tlog == nil {
		return nil
	}
	p := OperatorOverridePayload{
		OperatorID: operatorIDFromContext(ctx),
		Action:     action,
		ParamsJSON: paramsJSON,
		Ts:         s.nowFn().Unix(),
	}
	body, err := p.MarshalBinary()
	if err != nil {
		return err
	}
	return s.tlog.Append(TLogRecord{
		Kind:    TLogKindOperatorOverride,
		Payload: body,
		Ts:      time.Unix(p.Ts, 0).UTC(),
	})
}
```

`OperatorOverridePayload` is defined in Task 2 — `{OperatorID, Action, ParamsJSON, Ts}` with `MarshalBinary`/`UnmarshalBinary`.

- [ ] **Step 4: Run, confirm PASS**

```bash
cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission/tracker
PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=1 ./internal/admission/...
```

- [ ] **Step 5: Commit**

```bash
git add tracker/internal/admission/operator_override.go \
        tracker/internal/admission/operator_override_test.go
git commit -m "$(cat <<'EOF'
feat(tracker/admission): operator-override audit trail helper

writeOperatorOverride appends a TLogKindOperatorOverride record carrying
{operator_id, action, params_json, ts}. Used by every admin POST/DELETE
in Task 10. operator_id comes from a context key set by the auth
middleware (or "anonymous" when missing).

No-op when the tlog is disabled — keeps tests that don't care about
audit trail concise.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 12: Prometheus metrics — Collector

- File: `metrics.go` + `metrics_test.go`
- `func (s *Subsystem) Collector() prometheus.Collector` — returns one Collector wrapping ~20 metrics:
  - Hot-path: `admission_decisions_total{result}`, `admission_queue_depth`, `admission_queue_drained_per_sec`, `admission_pressure`, `admission_supply_total_headroom`, `admission_demand_rate_ewma`, `admission_decision_duration_seconds` (histogram)
  - Attestations: `admission_attestations_issued_total`, `admission_attestation_validation_failures_total{stage}`, `admission_attestation_age_seconds`, `admission_trial_tier_decisions_total`
  - Persistence: `admission_tlog_replay_gap_entries`, `admission_tlog_corruption_records_total`, `admission_snapshot_load_failures_total{which}`, `admission_degraded_mode_active`
  - Operational: `admission_clock_jump_detected_total{direction}`, `admission_fetchheadroom_timeouts_total`, `admission_rejections_total{reason}`, `admission_pressure_threshold_crossing{direction}`
- Decide / IssueAttestation / SupplySnapshot publication / replay all bump these. Add hooks to existing files — minimal touches.
- Tests: registry round-trip via `prometheus.NewPedanticRegistry()`; assert metric increments after driving the corresponding action.

### Task 13: Acceptance — §10 #9-12 (persistence/recovery)

- File: `acceptance_test.go`
- #9: Crash mid-OnLedgerEvent: drive 10 events, kill writer mid-flight via `panic` in fake `tlogWriter`, restart with replay, verify state catches up to ledger.
- #10: tlog mid-record corruption: write 5 records, flip a byte in the second, restart, verify replay applies records 1, halts, surfaces error, decisions still flow (but downgraded).
- #11: Latest snapshot deleted: emit 3 snapshots, delete the latest, restart, verify next-older loads + tlog replay catches up.
- #12: All snapshots corrupted: emit 3, corrupt all, restart, verify `DegradedMode() == true` + decisions still flow.

### Task 14: Acceptance — §10 #13-16 (performance benchmarks)

- File: `perf_bench_test.go`
- #13: `BenchmarkDecide_NoAttestation` + `BenchmarkDecide_WithAttestation`. Assert `b.NsPerOp() < 1_000_000` (1 ms) and `< 2_000_000` (2 ms) respectively. Run with `-benchtime=1s`.
- #14: `TestSustained100PerSec_NoMetricDegradation` — drive 100 Decide/sec for 5 simulated seconds (compressed via fake clock), assert tlog steady-state catches up + queue depth stable.
- #15: Aggregator-tick + heartbeat-headroom-change → SupplySnapshot updated within 5s of registry change. (Already covered by plan 2's TestAggregator_TickProducesSnapshot; this task adds an explicit acceptance test.)
- #16: tlog write rate ≤ ledger commit rate; observe over 5s simulation.

### Task 15: Acceptance — §10 #17-20 (security) + final integration

- File: `acceptance_test.go` (extend) + final `make check` + branch push.
- #17: Forged attestation (valid issuer, body tampered post-sign) → `Decide` returns ADMIT/QUEUE/REJECT with score = trial-tier (signature verification rejects).
- #18: Federation peer ejected after issuing attestation → admission falls through; `admission_attestation_validation_failures_total{stage="issuer_ejected"}` bumps.
- #19: Inflated peer attestation → score clamped to `cfg.MaxAttestationScoreImported`.
- #20: `/admission/queue` admin endpoint requires operator auth — Test wraps mux with `BasicAuthGuard`; without token returns 401; with token returns 200.
- Run from repo root:
  ```bash
  cd /Users/dor.amid/.superset/worktrees/token-bay/tracker_admission
  PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" make check
  PATH="$HOME/go/bin:$HOME/.local/share/mise/shims:$PATH" go test -race -count=10 ./internal/admission/...
  ```
- Push: `git push -u origin tracker_admission_persistence`.
- PR title: `feat(tracker/admission): persistence + admin + acceptance hardening`.

---

## Coverage of admission-design §10 acceptance criteria — what plan 3 covers

| §10 # | Criterion | Implemented in |
|---|---|---|
| 1-8 | (Functional) | Plan 2 |
| 9 | Crash mid-OnLedgerEvent → tlog catches up | Task 13 |
| 10 | tlog mid-record corruption → truncate + replay forward | Tasks 4, 7, 9, 13 |
| 11 | Latest snapshot deleted → next-older + replay; recovery < 30s | Task 13 |
| 12 | All snapshots corrupted → degraded mode | Tasks 9, 13 |
| 13 | `admission_decision_duration_seconds` p99 < 1ms / 2ms | Task 14 |
| 14 | Sustained 100 broker_requests/sec, no degradation | Task 14 |
| 15 | `admission_supply_total_headroom` updates within 5s | Task 14 |
| 16 | tlog write rate ≤ ledger commit rate | Task 14 |
| 17 | Forged attestation rejected | Task 15 |
| 18 | Ejected peer rejected | Task 15 |
| 19 | Inflated peer score clamped | Task 15 |
| 20 | `/admission/queue` requires operator auth | Tasks 10, 15 |
| Race | `go test -race` clean | Task 15 |

## Out-of-scope summary (deferred to other plans)

- Admin HTTP server skeleton (listener, TLS, real auth middleware) — separate tracker control-plane plan; plan 3 ships handlers as `http.Handler` instances + `BasicAuthGuard` helper.
- Real ledger event-bus emission — separate ledger plan; plan 3 still drives `OnLedgerEvent` via tests, but the §7.1 ledger cross-check uses an injected `LedgerSource` interface that the future ledger-bus plan will wire.
- Broker integration — separate plan.
- FetchHeadroom RPC — separate plan.
- Real federation peer-set — separate plan; plan 3's `BlocklistedPeers` admin endpoints operate on `cfg.AttestationPeerBlocklist` (already in tracker config).

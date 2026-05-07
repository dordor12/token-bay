# `plugin/internal/config` and `plugin/internal/auditlog` — Design

| | |
|---|---|
| Status | Approved 2026-05-07 |
| Owner | plugin |
| Depends on | plugin spec §8, §9 (`docs/superpowers/specs/plugin/2026-04-22-plugin-design.md`) |
| Mirrors | `tracker/internal/config` (same loader pattern) |

## 1. Scope

Two leaf packages under `plugin/internal/`:

- `config/` — YAML loader for `~/.token-bay/config.yaml`. Used by every other plugin subsystem to read its section.
- `auditlog/` — append-only writer for `~/.token-bay/audit.log`. Used by the consumer fallback path and the seeder bridge to record one entry per request.

Both are pure: no goroutines, no network, no IPC. Lifecycle (open/close, hot-reload, file watch) is owned by the sidecar.

## 2. `plugin/internal/config`

### 2.1 Public API

```go
func Load(path string) (*Config, error)            // Parse + ApplyDefaults + Validate
func Parse(r io.Reader) (*Config, error)           // YAML decode only; rejects unknown fields
func ApplyDefaults(c *Config)                      // idempotent default-fill + ~ expansion
func Validate(c *Config) error                     // accumulating *ValidationError
func DefaultConfig() *Config                       // defaults only; required fields zero-valued
```

Errors are typed: `*ParseError` (wraps yaml decode + unknown-field rejection, carries `Path` if loaded from disk) and `*ValidationError` (slice of `FieldError` with `Field`/`Message`). `errors.As`-comparable.

### 2.2 Schema

```yaml
role: both                          # consumer | seeder | both                  REQUIRED
tracker: auto                       # auto | <https/quic URL>                   REQUIRED
identity_key_path: ~/.token-bay/identity.key
audit_log_path:    ~/.token-bay/audit.log

cc_bridge:
  claude_bin: claude
  extra_flags: []
  sandbox:
    enabled: false
    driver: bubblewrap              # bubblewrap | firejail | docker

consumer:
  network_mode_ttl: 15m             # plugin spec §5.5

seeder:
  headroom_window: 15m              # plugin spec §6.3

idle_policy:
  mode: scheduled                   # scheduled | always_on
  window: "02:00-06:00"             # required when mode=scheduled
  activity_grace: 10m

privacy_tier: standard              # standard | tee_required | tee_preferred
max_spend_per_hour: 500
```

Durations parsed by `time.ParseDuration` (`15m`, `2h`, `30s`). Time-of-day windows are `HH:MM-HH:MM` (24-hour, local time, may wrap past midnight).

### 2.3 Required vs optional

**Required** (zero-valued ⇒ `Validate` flags it):
- `role`
- `tracker`

Everything else has a default. Identity-key and audit-log paths default to `~/.token-bay/identity.key` and `~/.token-bay/audit.log` and are tilde-expanded in `ApplyDefaults`.

### 2.4 Validation rules

`Validate` runs every rule, accumulating one `FieldError` per failure (operators see all problems at once). Order matches the order of fields in `Config` for stable diffs.

| Rule | Field |
|---|---|
| `role ∈ {consumer, seeder, both}` | `role` |
| `tracker == "auto"` OR a URL with scheme `https`/`quic` | `tracker` |
| `cc_bridge.claude_bin` non-empty | `cc_bridge.claude_bin` |
| If `cc_bridge.sandbox.enabled` then `driver ∈ {bubblewrap, firejail, docker}` | `cc_bridge.sandbox.driver` |
| `consumer.network_mode_ttl > 0` | `consumer.network_mode_ttl` |
| `seeder.headroom_window > 0` | `seeder.headroom_window` |
| `idle_policy.mode ∈ {scheduled, always_on}` | `idle_policy.mode` |
| If mode=scheduled: `idle_policy.window` parses as `HH:MM-HH:MM`, both endpoints valid 24-hour times | `idle_policy.window` |
| `idle_policy.activity_grace > 0` | `idle_policy.activity_grace` |
| `privacy_tier ∈ {standard, tee_required, tee_preferred}` | `privacy_tier` |
| `max_spend_per_hour >= 0` | `max_spend_per_hour` |
| `audit_log_path` and `identity_key_path` absolute (after `~` expansion) | each path field |

No filesystem checks at validate time (callers may bring up a fresh home dir). The audit-log opener and identity loader handle file errors at use time.

### 2.5 Hot-reload

Out of scope for this design. Plugin spec §9 mentions SIGHUP for non-structural fields; the sidecar will own that loop and call `Load` again. `doc.go` flags this as a `// TODO §9` so the next contributor knows where to wire it.

### 2.6 Source layout

```
plugin/internal/config/
├── CLAUDE.md
├── doc.go
├── config.go            -- Config + section types + DefaultConfig
├── load.go              -- Parse, Load
├── apply_defaults.go    -- ApplyDefaults (incl. ~ expansion)
├── validate.go          -- Validate + every check<Section>
├── errors.go            -- ParseError, ValidationError, FieldError
├── *_test.go            -- one per source file
└── testdata/
    ├── minimal.yaml
    ├── full.yaml
    ├── unknown_field.yaml
    └── invalid_<rule>.yaml × N
```

## 3. `plugin/internal/auditlog`

### 3.1 Public API

```go
type Logger struct { /* opaque */ }

func Open(path string) (*Logger, error)
func (l *Logger) LogConsumer(r ConsumerRecord) error
func (l *Logger) LogSeeder(r SeederRecord) error
func (l *Logger) Rotate() error
func (l *Logger) Close() error

// reader for /token-bay logs
func Read(path string) iter.Seq2[Record, error]   // streams one record per line

type Record interface { isRecord() }
type ConsumerRecord struct {
    RequestID     string
    ServedLocally bool
    SeederID      string    // empty when ServedLocally
    CostCredits   int64
    Timestamp     time.Time
}
type SeederRecord struct {
    RequestID        string
    Model            string
    InputTokens      int
    OutputTokens     int
    ConsumerIDHash   [32]byte
    StartedAt        time.Time
    CompletedAt      time.Time
    TrackerEntryHash *[32]byte
}
type UnknownRecord struct { Kind string; Raw json.RawMessage }
```

### 3.2 File format

One JSON object per line, terminated by `\n`. Discriminator field `kind` (`"consumer"` | `"seeder"`). `[32]byte` fields encode as lowercase hex strings. Times encode as RFC3339Nano UTC.

```json
{"kind":"consumer","request_id":"...","served_locally":true,"cost_credits":0,"timestamp":"..."}
{"kind":"seeder","request_id":"...","model":"sonnet-4","input_tokens":120,"output_tokens":340,"consumer_id_hash":"ab12...","started_at":"...","completed_at":"..."}
```

Forward-compat: a reader sees an unknown `kind` and yields `UnknownRecord` instead of erroring. New record kinds can be added without breaking old binaries.

### 3.3 Append discipline

- `Open` calls `os.OpenFile(path, O_APPEND|O_CREATE|O_WRONLY, 0o600)`. If `path` already exists with stricter mode, leave it alone.
- Each `Log*` call: marshals to JSON, appends `\n`, writes the full line in **one** `Write` (so concurrent appenders interleave at line boundaries on POSIX), then `fsync`. A `sync.Mutex` serializes writes from the same `*Logger`.
- Never truncate. Never rewrite. `Rotate` is the only way to break out of the current file.

### 3.4 Rotation

`Rotate()`:
1. Acquires the logger mutex.
2. `Close`s the current fd.
3. `os.Rename(path, path + "." + time.Now().UTC().Format("20060102T150405Z"))` — POSIX atomic.
4. Re-opens `path` fresh.

The original file is kept in place so `Open` is a no-op for callers reading the live tail. Rotation cadence is operator-driven for v1 (no auto-rotation by size or age). 90-day retention is enforced externally (logrotate or a future cron); this package never deletes.

### 3.5 Reader

`Read(path)` returns `iter.Seq2[Record, error]`. Iteration:

- Opens `path` read-only, streams via `bufio.Scanner` with a 1 MiB max line buffer.
- For each line: peek `kind`, dispatch into the matching struct, yield `(record, nil)`. On JSON error, yield `(nil, err)` and continue (a single corrupt line doesn't kill the stream).
- Caller using Go 1.23+ range-over-func, e.g.:

```go
for rec, err := range auditlog.Read(p) {
    ...
}
```

### 3.6 Concurrency model

Single-process, single-`Logger` per file. Two `Logger`s on the same path is undefined — interleaved writes at line boundaries are not guaranteed across distinct fds. The sidecar opens exactly one `Logger` for the audit file at startup and shares it across roles.

### 3.7 Source layout

```
plugin/internal/auditlog/
├── CLAUDE.md
├── doc.go
├── records.go           -- record types + JSON tags + (un)marshal helpers
├── logger.go            -- Open, Logger, LogConsumer, LogSeeder, Rotate, Close
├── reader.go            -- Read iterator
├── errors.go            -- typed errors
└── *_test.go            -- one per source file
```

## 4. Out of scope for v1

- SIGHUP hot-reload (sidecar concern; package exposes `Load` for re-call).
- Auto-rotation by size or age. Operator-driven only.
- Compression of rotated files.
- 90-day retention purge. External cron / logrotate.
- Env-var or flag overrides on top of YAML. Discuss in an issue first if needed.
- `${var}` template engine. `~` expansion is one line of Go.

## 5. Acceptance

- `Load` of `testdata/minimal.yaml` plus a tempdir-resident path setup returns a valid `*Config` with defaults applied.
- `Load` of `testdata/full.yaml` round-trips every spec §9 field.
- Every invalid fixture under `testdata/invalid_*.yaml` produces a `*ValidationError` whose `Errors` includes the named field.
- `auditlog.Open` + parallel `LogConsumer` / `LogSeeder` from N goroutines yields exactly N lines, each parseable, each with a complete record. No partial lines, no interleaved bytes.
- `Rotate` while writes are in flight produces two well-formed files: rotated archive ends mid-batch on a record boundary, fresh file picks up after.
- `Read` over a 1k-record fixture yields all records in order, dispatches consumer/seeder correctly, and treats an unknown `kind` as `UnknownRecord`.
- `make -C plugin check` is green.

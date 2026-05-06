# plugin/internal/auditlog — Development Context

## What this is

Append-only JSONL writer for `~/.token-bay/audit.log`. Per plugin spec §8 the consumer side records one entry per fallback turn and the seeder side records one entry per forwarded request. **No prompt or response content is ever logged** — only metering metadata.

Public API (do not break without coordinating callers):

| Symbol | Purpose |
|---|---|
| `Open(path) (*Logger, error)` | O_APPEND open, mode 0600 on first create |
| `(*Logger).LogConsumer(rec)` | append one consumer record + fsync |
| `(*Logger).LogSeeder(rec)` | append one seeder record + fsync |
| `(*Logger).Rotate()` | rename current file aside, open fresh |
| `(*Logger).Close()` | idempotent close |
| `Read(path) iter.Seq2[Record, error]` | range-over-func reader for `/token-bay logs` |

Source: `records.go`, `logger.go`, `reader.go`, `errors.go`, `doc.go`. One test file per source file.

## Non-negotiable rules

1. **Never truncate, never rewrite.** Plugin CLAUDE.md rule #4. `Rotate` is the *only* file-level mutation; it renames the live file aside (POSIX-atomic) and opens path fresh.
2. **No content fields.** Records carry tokens, hashes, and identifiers — never prompts, responses, or headers. If you find yourself adding a content field, stop and re-read plugin spec §8.
3. **One JSON object per line.** A single `Write` call per record; the OS appends atomically under O_APPEND for sub-PIPE_BUF lines, and the Logger mutex protects within-process serialization.
4. **`Read` does not error on a single corrupt line.** The iterator yields `(nil, err)` for that line and continues — a corrupt entry never silently truncates a multi-megabyte log.
5. **Forward-compat: unknown `kind` ⇒ `UnknownRecord`, not error.** Older tooling must keep working against newer logs.

## Adding a new record kind

1. Add a `Kind*` constant in `records.go`.
2. Add the public record struct (POD only — no embedded interfaces, no time pointers without good reason).
3. Add a private wire struct with explicit `json:` tags. Snake-case keys. RFC3339Nano UTC for times. Hex strings for `[N]byte`.
4. Wire `marshalRecord` and `unmarshalRecord` switch arms.
5. Tests: round-trip happy path, optional-field-omitted variant, decoder behavior on malformed value.

## Things that look surprising and aren't bugs

- The `Logger` keeps a single fd open across many `LogConsumer`/`LogSeeder` calls. Re-opening per call would be safer for crash atomicity but kills throughput; the fsync-per-write covers the durability argument.
- `Rotate` returns `ErrClosed` if called after `Close` — even though the file still exists on disk, the *Logger* is no longer the live writer for it.
- `time.Time` fields encode in UTC even if the caller passes a local-zone time. The audit log is operator-readable across regions; UTC is the only sensible default.
- The package depends on stdlib only (no shared/, no zerolog). Logging-of-logs would be a layering hazard.

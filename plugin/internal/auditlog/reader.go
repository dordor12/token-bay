package auditlog

import (
	"bufio"
	"fmt"
	"iter"
	"os"
)

// readMaxLineBytes is the largest accepted JSON line. Records carry no
// content (plugin spec §8); the limit only exists so a corrupt log
// doesn't trigger unbounded allocation.
const readMaxLineBytes = 1 << 20 // 1 MiB

// Read returns an iterator over the records in the audit log at path.
//
// Behavior:
//   - On open failure (missing file, permission denied, etc.) the
//     iterator yields exactly one (nil, error) and stops.
//   - One corrupt line yields one (nil, error) but iteration continues
//     past it — a corrupt entry never silently truncates the stream.
//   - An unknown kind discriminator yields an UnknownRecord (no error)
//     so older tooling can still walk newer logs.
//   - Empty lines are skipped.
//
// Iteration uses Go 1.23 range-over-func:
//
//	for rec, err := range auditlog.Read(path) { ... }
func Read(path string) iter.Seq2[Record, error] {
	return func(yield func(Record, error) bool) {
		f, err := os.Open(path) //nolint:gosec // operator-supplied path; reading audit log
		if err != nil {
			yield(nil, fmt.Errorf("auditlog: open %s: %w", path, err))
			return
		}
		defer f.Close()

		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), readMaxLineBytes)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			rec, err := unmarshalRecord(line)
			if err != nil {
				if !yield(nil, fmt.Errorf("auditlog: parse line: %w", err)) {
					return
				}
				continue
			}
			if !yield(rec, nil) {
				return
			}
		}
		if err := sc.Err(); err != nil {
			yield(nil, fmt.Errorf("auditlog: scan %s: %w", path, err))
		}
	}
}

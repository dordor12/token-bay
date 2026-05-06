package auditlog

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"sync"
	"time"
)

// Logger appends audit-log entries to a single file. Safe for concurrent
// use within one process; two Loggers writing the same path is undefined
// (interleaved writes across distinct fds are not line-atomic in general).
//
// Plugin policy: never truncate, never rewrite. Rotate is the only way to
// retire a file. See plugin CLAUDE.md rule #4.
type Logger struct {
	path string
	now  func() time.Time // injectable for tests; defaults to time.Now

	mu sync.Mutex
	f  *os.File // nil after Close
}

// Open opens path for O_APPEND with mode 0600 if it does not exist. If
// the file already exists, its mode is preserved and existing content
// is left in place (audit log is append-only).
func Open(path string) (*Logger, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("auditlog: open %s: %w", path, err)
	}
	return &Logger{path: path, f: f, now: time.Now}, nil
}

// Rotate closes the current file, renames it to <path>.<UTC-timestamp>Z,
// and re-opens path fresh. The original path is the live tail; the
// archive is the retired one. Atomic on POSIX (rename(2)).
//
// On a sub-second collision, a -<n> suffix is appended.
func (l *Logger) Rotate() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return ErrClosed
	}
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("auditlog: close before rotate: %w", err)
	}
	l.f = nil

	archive, err := uniqueArchivePath(l.path, l.now())
	if err != nil {
		return fmt.Errorf("auditlog: pick archive name: %w", err)
	}
	if err := os.Rename(l.path, archive); err != nil {
		return fmt.Errorf("auditlog: rename for rotate: %w", err)
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("auditlog: re-open after rotate: %w", err)
	}
	l.f = f
	return nil
}

// uniqueArchivePath builds a non-colliding rotated path of the form
// "<base>.YYYYMMDDTHHMMSSZ" (UTC) — appending a "-N" disambiguator if a
// prior rotation in the same second already claimed the bare name.
func uniqueArchivePath(base string, now time.Time) (string, error) {
	stem := base + "." + now.UTC().Format("20060102T150405Z")
	if _, err := os.Stat(stem); errors.Is(err, fs.ErrNotExist) {
		return stem, nil
	} else if err != nil {
		return "", err
	}
	for i := 1; i < 10000; i++ {
		candidate := stem + "-" + strconv.Itoa(i)
		if _, err := os.Stat(candidate); errors.Is(err, fs.ErrNotExist) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("auditlog: exhausted rotation suffixes for %s", stem)
}

// Path returns the absolute path the Logger was opened against. Stable
// across Rotate (Rotate re-opens the same path).
func (l *Logger) Path() string {
	return l.path
}

// LogConsumer appends a consumer-side record as one JSON line + fsync.
func (l *Logger) LogConsumer(rec ConsumerRecord) error {
	return l.append(rec)
}

// LogSeeder appends a seeder-side record as one JSON line + fsync.
func (l *Logger) LogSeeder(rec SeederRecord) error {
	return l.append(rec)
}

// Close closes the underlying file. Idempotent. Subsequent Log* / Rotate
// calls return ErrClosed.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

func (l *Logger) append(rec Record) error {
	line, err := marshalRecord(rec)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return ErrClosed
	}
	if _, err := l.f.Write(line); err != nil {
		return fmt.Errorf("auditlog: write: %w", err)
	}
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("auditlog: fsync: %w", err)
	}
	return nil
}

package auditlog

import (
	"fmt"
	"os"
	"sync"
)

// Logger appends audit-log entries to a single file. Safe for concurrent
// use within one process; two Loggers writing the same path is undefined
// (interleaved writes across distinct fds are not line-atomic in general).
//
// Plugin policy: never truncate, never rewrite. Rotate is the only way to
// retire a file. See plugin CLAUDE.md rule #4.
type Logger struct {
	path string

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
	return &Logger{path: path, f: f}, nil
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

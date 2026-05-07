package auditlog

import "errors"

// ErrClosed is returned by LogConsumer, LogSeeder, and Rotate after the
// Logger has been Closed. The audit log is permanent state — callers
// should treat ErrClosed as a programming bug, not a recoverable signal.
var ErrClosed = errors.New("auditlog: logger is closed")

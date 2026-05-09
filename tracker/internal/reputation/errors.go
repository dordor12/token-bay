package reputation

import "errors"

// ErrSubsystemClosed is returned by methods called after Close.
var ErrSubsystemClosed = errors.New("reputation: subsystem closed")

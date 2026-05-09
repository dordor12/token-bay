package sidecar

import "errors"

// ErrInvalidDeps is returned by New when the supplied Deps fail Validate.
var ErrInvalidDeps = errors.New("sidecar: invalid deps")

// ErrAlreadyStarted is returned by Run on the second call.
var ErrAlreadyStarted = errors.New("sidecar: already started")

// ErrClosed is returned by Run after Close has been called.
var ErrClosed = errors.New("sidecar: closed")

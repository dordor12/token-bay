package config

import (
	"fmt"
	"strings"
)

// ParseError reports a YAML decode failure or an unknown-key rejection.
// Path is empty when the source was an io.Reader (Parse) and populated
// when the source was a file (Load).
type ParseError struct {
	Path string
	Err  error
}

func (e *ParseError) Error() string {
	if e.Path == "" {
		return fmt.Sprintf("config: parse error: %v", e.Err)
	}
	return fmt.Sprintf("config: parse error in %s: %v", e.Path, e.Err)
}

func (e *ParseError) Unwrap() error { return e.Err }

// FieldError describes a single invariant failure produced by Validate.
type FieldError struct {
	Field   string
	Message string
}

func (e FieldError) String() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// ValidationError aggregates every invariant failure from a Validate call.
// One indented line per FieldError. No header — callers (CLI in particular)
// emit their own "validation failed:" line before iterating Errors.
type ValidationError struct {
	Errors []FieldError
}

func (e *ValidationError) Error() string {
	if len(e.Errors) == 0 {
		return "config: validation failed (no specific errors recorded)"
	}
	parts := make([]string, 0, len(e.Errors))
	for _, fe := range e.Errors {
		parts = append(parts, "  - "+fe.String())
	}
	return strings.Join(parts, "\n")
}

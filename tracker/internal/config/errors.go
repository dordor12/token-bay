package config

import (
	"fmt"
	"strings"
)

// ParseError reports a YAML decode failure or an unknown-key rejection.
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
type ValidationError struct {
	Errors []FieldError
}

// Error returns one indented line per FieldError. Deliberately no
// summary header — callers (CLI in particular) emit their own context
// line before iterating Errors, and a header here would cause double-
// labelling. For ad-hoc logging the bare list is still readable.
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

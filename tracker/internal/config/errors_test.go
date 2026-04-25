package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseError_ErrorIncludesPathAndCause(t *testing.T) {
	cause := errors.New("yaml: line 3: mapping values are not allowed in this context")
	pe := &ParseError{Path: "/etc/token-bay/tracker.yaml", Err: cause}

	msg := pe.Error()

	assert.Contains(t, msg, "/etc/token-bay/tracker.yaml")
	assert.Contains(t, msg, "line 3")
}

func TestParseError_EmptyPathOmitsPathSegment(t *testing.T) {
	pe := &ParseError{Err: errors.New("oops")}

	msg := pe.Error()

	assert.NotContains(t, msg, "/")
	assert.Contains(t, msg, "oops")
}

func TestParseError_Unwrap(t *testing.T) {
	cause := errors.New("inner")
	pe := &ParseError{Err: cause}

	assert.Same(t, cause, errors.Unwrap(pe))
}

func TestValidationError_ErrorListsAllFieldErrors(t *testing.T) {
	ve := &ValidationError{
		Errors: []FieldError{
			{Field: "data_dir", Message: "must be non-empty"},
			{Field: "admission.score_weights", Message: "weights sum to 0.97, must be 1.0 ± 0.001"},
		},
	}

	msg := ve.Error()

	assert.Contains(t, msg, "data_dir")
	assert.Contains(t, msg, "admission.score_weights")
	assert.Contains(t, msg, "weights sum to 0.97")
	// Each FieldError on its own line for operator readability:
	assert.Equal(t, 2, strings.Count(msg, "\n")+1-strings.Count(msg, "\n\n"))
}

func TestValidationError_ZeroErrorsStillFormats(t *testing.T) {
	ve := &ValidationError{}

	msg := ve.Error()

	require.NotEmpty(t, msg)
}

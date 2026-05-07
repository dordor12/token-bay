package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseError_WithPath(t *testing.T) {
	err := &ParseError{Path: "/etc/cfg.yaml", Err: errors.New("bad token")}
	assert.Contains(t, err.Error(), "/etc/cfg.yaml")
	assert.Contains(t, err.Error(), "bad token")
}

func TestParseError_WithoutPath(t *testing.T) {
	err := &ParseError{Err: errors.New("bad token")}
	assert.Equal(t, "config: parse error: bad token", err.Error())
}

func TestParseError_UnwrapsInner(t *testing.T) {
	inner := errors.New("inner")
	err := &ParseError{Err: inner}
	require.True(t, errors.Is(err, inner))
}

func TestValidationError_FormatsOneLinePerFieldError(t *testing.T) {
	err := &ValidationError{Errors: []FieldError{
		{Field: "role", Message: "must be one of consumer|seeder|both"},
		{Field: "tracker", Message: "must be \"auto\" or a URL"},
	}}
	out := err.Error()
	require.Equal(t, 2, strings.Count(out, "\n")+1)
	assert.Contains(t, out, "  - role: must be one of consumer|seeder|both")
	assert.Contains(t, out, "  - tracker: must be \"auto\" or a URL")
	assert.False(t, strings.HasPrefix(out, "validation failed"),
		"no header — caller emits its own context line")
}

func TestValidationError_EmptyErrorsHasFallbackString(t *testing.T) {
	err := &ValidationError{}
	assert.Equal(t,
		"config: validation failed (no specific errors recorded)",
		err.Error(),
	)
}

func TestFieldError_StringIsFieldColonMessage(t *testing.T) {
	fe := FieldError{Field: "max_spend_per_hour", Message: "must be >= 0"}
	assert.Equal(t, "max_spend_per_hour: must be >= 0", fe.String())
}

//go:build windows

package ccbridge

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExecRunner_Windows_Unsupported(t *testing.T) {
	runner := &ExecRunner{BinaryPath: "claude.exe"}
	var sink bytes.Buffer
	err := runner.Run(context.Background(), Request{
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
		Model:    "x",
	}, &sink)
	assert.True(t, errors.Is(err, ErrUnsupportedPlatform))
}

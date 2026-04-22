package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRootCmd_Version_PrintsExpected(t *testing.T) {
	var buf bytes.Buffer
	cmd := newRootCmd()
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"version"})

	err := cmd.Execute()

	assert.NoError(t, err)
	assert.Contains(t, buf.String(), "token-bay-sidecar")
}

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
	assert.Contains(t, buf.String(), "token-bay-tracker")
	assert.Contains(t, buf.String(), "0.0.0-dev")
}

func TestRootCmd_ConfigSubcommandRegistered(t *testing.T) {
	cmd := newRootCmd()

	found := false
	for _, sub := range cmd.Commands() {
		if sub.Name() == "config" {
			found = true
			haveValidate := false
			for _, sub2 := range sub.Commands() {
				if sub2.Name() == "validate" {
					haveValidate = true
				}
			}
			assert.True(t, haveValidate, "expected 'validate' under 'config'")
		}
	}
	assert.True(t, found, "expected 'config' subcommand registered")
}

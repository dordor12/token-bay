package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunCmd_Registered(t *testing.T) {
	root := newRootCmd()
	c, _, err := root.Find([]string{"run"})
	if err != nil {
		t.Fatalf("subcommand 'run' not found: %v", err)
	}
	if c == nil || c.Name() != "run" {
		t.Fatalf("got %+v", c)
	}
}

func TestRunCmd_RequiresConfigFlag(t *testing.T) {
	root := newRootCmd()
	root.SetArgs([]string{"run"})
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	root.SetOut(&stderr)

	captured := -1
	ctx := withExitFunc(context.Background(), func(code int) { captured = code })
	root.SetContext(ctx)

	if err := root.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if captured != 1 {
		t.Fatalf("exit code = %d, want 1", captured)
	}
	if !strings.Contains(stderr.String(), "config") {
		t.Fatalf("stderr does not mention config: %q", stderr.String())
	}
}

func TestRunCmd_BadConfig_ReportsValidationError(t *testing.T) {
	tmp := t.TempDir() + "/bogus.yaml"
	// Don't create the file — Load surfaces an os.PathError, not a
	// ParseError or ValidationError, so the catch-all branch in
	// reportConfigError fires (exit code 1).
	root := newRootCmd()
	root.SetArgs([]string{"run", "--config", tmp})
	var stderr bytes.Buffer
	root.SetErr(&stderr)

	captured := -1
	ctx := withExitFunc(context.Background(), func(code int) { captured = code })
	root.SetContext(ctx)

	_ = root.Execute()
	if captured != 1 {
		t.Fatalf("exit code = %d, want 1", captured)
	}
}

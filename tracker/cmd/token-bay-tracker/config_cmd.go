package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/token-bay/token-bay/tracker/internal/config"
)

// exitFuncKey is the context key under which the exit hook is stored.
// Tests put a capturing function here; main.go (Task 16) puts os.Exit.
type exitFuncKey struct{}

// newConfigCmd returns the `config` subcommand tree. Today only `validate`
// is exposed; future subcommands (`show`, `defaults`) belong here.
func newConfigCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "config",
		Short: "Inspect and validate tracker configuration",
	}
	root.AddCommand(newConfigValidateCmd())
	return root
}

func newConfigValidateCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Parse and validate a tracker config file; exit non-zero on failure",
		RunE: func(cmd *cobra.Command, args []string) error {
			if configPath == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "error: --config is required")
				exitWithCode(cmd.Context(), 1)
				return nil
			}
			c, err := config.Load(configPath)
			if err != nil {
				return reportConfigError(cmd, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "OK: %s (%d sections)\n", configPath, sectionCount(c))
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to tracker.yaml (required)")
	return cmd
}

// reportConfigError dispatches a Load error to the right exit code per the
// design spec §3.3:
//   - *ParseError      → 2
//   - *ValidationError → 3
//   - other (file missing, permission, …) → 1
func reportConfigError(cmd *cobra.Command, err error) error {
	stderr := cmd.ErrOrStderr()
	var pe *config.ParseError
	if errors.As(err, &pe) {
		fmt.Fprintf(stderr, "parse error: %s\n", pe.Error())
		exitWithCode(cmd.Context(), 2)
		return nil
	}
	var ve *config.ValidationError
	if errors.As(err, &ve) {
		fmt.Fprintln(stderr, "validation failed:")
		for _, fe := range ve.Errors {
			fmt.Fprintf(stderr, "  - %s\n", fe.String())
		}
		exitWithCode(cmd.Context(), 3)
		return nil
	}
	fmt.Fprintln(stderr, err.Error())
	exitWithCode(cmd.Context(), 1)
	return nil
}

// sectionCount returns the number of top-level Config sections.
func sectionCount(_ *config.Config) int {
	// Server, Admin, Ledger, Broker, Settlement, Federation, Reputation,
	// Admission, STUNTURN, Metrics — ten composite sections.
	return 10
}

// exitWithCode looks up the exit hook on cmd.Context() and calls it. In
// tests the hook is set by withExitFunc so the test captures the code.
// In production main.go puts os.Exit on the context.
func exitWithCode(ctx context.Context, code int) {
	if ctx == nil {
		return
	}
	v := ctx.Value(exitFuncKey{})
	if fn, ok := v.(func(int)); ok {
		fn(code)
	}
}

// withExitFunc returns a context with fn registered as the exit hook.
// Used by main.go and by tests.
func withExitFunc(parent context.Context, fn func(int)) context.Context {
	return context.WithValue(parent, exitFuncKey{}, fn)
}

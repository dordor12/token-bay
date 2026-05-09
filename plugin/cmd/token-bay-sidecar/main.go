// Package main is the token-bay-sidecar entry point.
//
// The sidecar is the long-lived process behind the Token-Bay Claude Code
// plugin. It coordinates the tracker connection, consumer-side fallback
// pipeline, and seeder-side bridge invocation.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

const version = "0.0.0-dev"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "token-bay-sidecar",
		Short: "Token-Bay plugin sidecar",
	}
	root.AddCommand(newVersionCmd())
	root.AddCommand(newRunCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the sidecar version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "token-bay-sidecar %s\n", version)
		},
	}
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

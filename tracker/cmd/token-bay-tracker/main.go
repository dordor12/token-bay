// Package main is the token-bay-tracker entry point.
//
// The tracker is the regional coordination server: seeder registry, request
// broker, credit ledger owner, STUN/TURN rendezvous, federation peer.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

const version = "0.0.0-dev"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "token-bay-tracker",
		Short: "Token-Bay regional tracker server",
	}
	root.AddCommand(newVersionCmd())
	root.AddCommand(newConfigCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the tracker version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "token-bay-tracker %s\n", version)
		},
	}
}

func main() {
	root := newRootCmd()
	ctx := withExitFunc(context.Background(), os.Exit)
	root.SetContext(ctx)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

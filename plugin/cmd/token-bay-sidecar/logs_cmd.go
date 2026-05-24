package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/token-bay/token-bay/plugin/internal/auditlog"
	"github.com/token-bay/token-bay/plugin/internal/config"
)

const defaultLogsTail = 50

func newLogsCmd() *cobra.Command {
	var configPath string
	var tail int
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Tail the audit log (~/.token-bay/audit.log)",
		Long: `Reads the audit log directly from disk (no sidecar dependency) — the
log file is the source of truth, and being able to tail it while the
sidecar is down is a legitimate operator use case.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if configPath == "" {
				return errors.New("--config is required")
			}
			cfg, err := config.Load(configPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			return runLogs(cmd.OutOrStdout(), cmd.ErrOrStderr(), cfg.AuditLogPath, tail)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to ~/.token-bay/config.yaml (required)")
	cmd.Flags().IntVar(&tail, "tail", defaultLogsTail, "Number of trailing entries to print")
	return cmd
}

func runLogs(stdout, stderr io.Writer, auditPath string, tail int) error {
	if tail < 1 {
		tail = defaultLogsTail
	}
	// Ring-buffer the trailing N records — the audit log is append-only
	// so a forward scan is the only direction available.
	buf := make([]string, 0, tail)
	var iterErr error
	for rec, err := range auditlog.Read(auditPath) {
		if err != nil {
			iterErr = err
			continue
		}
		line := formatAuditRecord(rec)
		if line == "" {
			continue
		}
		if len(buf) == tail {
			buf = append(buf[1:], line)
		} else {
			buf = append(buf, line)
		}
	}
	if iterErr != nil && len(buf) == 0 {
		fmt.Fprintf(stderr, "read audit log: %v\n", iterErr)
		return fmt.Errorf("read audit log: %w", iterErr)
	}
	for _, line := range buf {
		if _, err := fmt.Fprintln(stdout, line); err != nil {
			return err
		}
	}
	return nil
}

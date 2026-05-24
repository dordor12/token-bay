package main

import (
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/token-bay/token-bay/plugin/internal/auditlog"
)

const auditTimeFormat = "2006-01-02T15:04:05Z07:00"

// sidecarHTTPTimeout caps every CLI ↔ sidecar local HTTP round-trip.
// Five seconds matches the hooks subcommand's host-turn ceiling — the
// query subcommands aren't latency-critical, but a stuck supervisor
// must not wedge the slash command.
const sidecarHTTPTimeout = 5 * time.Second

// renderKVTable writes a two-column aligned key/value table sorted by
// key. Used by status and balance to keep slash-command output
// deterministic.
func renderKVTable(w io.Writer, m map[string]any) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	width := 0
	for _, k := range keys {
		if len(k) > width {
			width = len(k)
		}
	}
	for _, k := range keys {
		if _, err := fmt.Fprintf(w, "%-*s  %v\n", width, k, m[k]); err != nil {
			return err
		}
	}
	return nil
}

// formatAuditRecord renders one audit-log record as a single human-
// readable line. Returns "" for record kinds the CLI does not know how
// to format (forward-compat UnknownRecord).
func formatAuditRecord(rec auditlog.Record) string {
	switch r := rec.(type) {
	case auditlog.ConsumerRecord:
		return fmt.Sprintf("%s  consumer  req=%s  local=%t  credits=%d",
			r.Timestamp.Format(auditTimeFormat), r.RequestID, r.ServedLocally, r.CostCredits)
	case auditlog.SeederRecord:
		return fmt.Sprintf("%s  seeder    req=%s  model=%s  in=%d out=%d",
			r.CompletedAt.Format(auditTimeFormat), r.RequestID, r.Model, r.InputTokens, r.OutputTokens)
	case auditlog.TransferRecord:
		return fmt.Sprintf("%s  transfer  req=%s  %s→%s  amt=%d  outcome=%s",
			r.Timestamp.Format(auditTimeFormat), r.RequestID, r.SourceRegion, r.DestRegion, r.Amount, r.Outcome)
	}
	return ""
}

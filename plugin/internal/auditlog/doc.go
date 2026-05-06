// Package auditlog implements the plugin's append-only audit log per
// plugin spec §8 (https://...specs/plugin/2026-04-22-plugin-design.md).
//
// One JSON object per line, kind discriminator on every record. Two
// concrete record types — ConsumerRecord and SeederRecord — plus an
// UnknownRecord forward-compat envelope. No prompt or response content
// is ever recorded; only metering metadata.
//
// The audit log is permanent. Open / LogConsumer / LogSeeder / Rotate
// never truncate the file. Rotate atomically renames the live file
// aside under a UTC-timestamp suffix and re-opens path fresh.
//
// Design: docs/superpowers/specs/plugin/2026-05-07-config-auditlog-design.md
//
// # Caveats
//
// 90-day retention enforcement and auto-rotation by size or age are
// out of scope for v1. Operators are expected to drive rotation via
// logrotate or a future cron — this package never deletes.
package auditlog

// Package config implements the YAML loader for the Token-Bay plugin.
//
// Public API: Load (parse + apply defaults + validate), Parse (YAML
// decode only), ApplyDefaults (idempotent default-fill + ~ expansion),
// Validate (accumulating *ValidationError), DefaultConfig.
//
// Mirrors tracker/internal/config — same Load/Parse/Validate split,
// same accumulating-error model, same explicit-no-reflection policy.
//
// Design: docs/superpowers/specs/plugin/2026-05-07-config-auditlog-design.md
// Schema source: plugin spec §9.
//
// # TODO §9: hot-reload
//
// Plugin spec §9 calls for SIGHUP-driven hot-reload of non-structural
// fields. The sidecar — not this package — owns that loop; it should
// re-call Load and dispatch the new *Config to subscribers. The
// boundary between "structural" and "non-structural" is sidecar policy.
package config

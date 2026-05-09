// Package sidecar is the long-lived supervisor for the token-bay-sidecar
// process. It composes the plugin's subsystems (identity, auditlog,
// trackerclient, ccproxy) into a single App with a Run/Close lifecycle.
//
// See docs/superpowers/specs/plugin/2026-04-22-plugin-design.md §2.1, §3.
package sidecar

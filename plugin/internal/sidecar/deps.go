package sidecar

import (
	"fmt"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/plugin/internal/auditlog"
	"github.com/token-bay/token-bay/plugin/internal/ccbridge"
	"github.com/token-bay/token-bay/plugin/internal/identity"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
)

// Deps is everything the supervisor needs to construct itself. The cmd
// layer (cmd/token-bay-sidecar/run_cmd.go) is responsible for building
// each field from disk-resident inputs (config, identity files, audit log
// path, bootstrap endpoints).
//
// Required fields must satisfy Validate; optional fields are documented
// inline.
type Deps struct {
	// Logger is the structured logger used by the supervisor and passed
	// down to subsystems that accept one. Required.
	Logger zerolog.Logger

	// Signer is the plugin's Ed25519 keypair holder, satisfying the
	// trackerclient.Signer interface. Built by internal/identity.Open.
	// Required.
	Signer *identity.Signer

	// AuditLog is the open append-only writer for ~/.token-bay/audit.log.
	// The supervisor closes it during shutdown. Required.
	AuditLog *auditlog.Logger

	// CCProxyAddr is the bind address for the local Anthropic-compatible
	// HTTP server (plugin spec §2.5). Use "127.0.0.1:0" to let the OS
	// pick a port — the resolved address surfaces via Status. Required.
	CCProxyAddr string

	// TrackerEndpoints is the bootstrap list handed to trackerclient.
	// Empty list is rejected. Required.
	TrackerEndpoints []trackerclient.TrackerEndpoint

	// TrackerClientOverrides lets the cmd layer tweak trackerclient.Config
	// fields (timeouts, backoff, etc.) before validation. Optional.
	TrackerClientOverrides func(*trackerclient.Config)

	// Janitor reaps stale per-client session folders under
	// ccbridge.ExecRunner.SeederRoot. Constructed by the cmd layer
	// with a Root, ActiveClientChecker, Grace, and Interval. Run as
	// a goroutine alongside ccproxy/trackerclient; stops on ctx
	// cancel. Optional — when nil the supervisor skips it (useful
	// for tests and for early deployments before a peer-tracker
	// exists). When the peer-tracker subsystem lands, the cmd
	// layer constructs it and wires it as the Checker so reaping
	// follows live connection state.
	Janitor *ccbridge.Janitor
}

// Validate enforces required-field invariants. Returns an ErrInvalidDeps
// chain on failure.
func (d Deps) Validate() error {
	if d.Signer == nil {
		return fmt.Errorf("%w: Signer must be non-nil", ErrInvalidDeps)
	}
	if d.AuditLog == nil {
		return fmt.Errorf("%w: AuditLog must be non-nil", ErrInvalidDeps)
	}
	if d.CCProxyAddr == "" {
		return fmt.Errorf("%w: CCProxyAddr must be non-empty", ErrInvalidDeps)
	}
	if len(d.TrackerEndpoints) == 0 {
		return fmt.Errorf("%w: TrackerEndpoints must be non-empty", ErrInvalidDeps)
	}
	return nil
}

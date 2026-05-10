package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/token-bay/token-bay/plugin/internal/auditlog"
	"github.com/token-bay/token-bay/plugin/internal/ccbridge"
	"github.com/token-bay/token-bay/plugin/internal/config"
	"github.com/token-bay/token-bay/plugin/internal/identity"
	"github.com/token-bay/token-bay/plugin/internal/sidecar"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
)

const (
	defaultCCProxyAddr = "127.0.0.1:0"
	envTrackerHash     = "TOKEN_BAY_TRACKER_HASH" //nolint:gosec // env var name, not a credential

	// Janitor defaults. Per-client session folders are reaped when
	// (a) their owning peer is no longer reported active by the
	// ActiveClientChecker AND (b) the folder hasn't been touched
	// within janitorGrace. Two-clause check protects briefly-
	// disconnected peers from losing their state.
	janitorGrace    = 30 * time.Minute
	janitorInterval = 5 * time.Minute
)

func newRunCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Start the token-bay sidecar",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if configPath == "" {
				return errors.New("--config is required")
			}
			cfg, err := config.Load(configPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			cfgDir := filepath.Dir(configPath)
			signer, _, err := identity.Open(cfgDir)
			if err != nil {
				return fmt.Errorf("open identity at %s (run /token-bay enroll first?): %w", cfgDir, err)
			}

			al, err := auditlog.Open(cfg.AuditLogPath)
			if err != nil {
				return fmt.Errorf("open audit log: %w", err)
			}

			endpoints, err := resolveTrackerEndpoints(cfg.Tracker)
			if err != nil {
				_ = al.Close()
				return err
			}

			seederRoot := filepath.Join(cfgDir, "seeder-sessions")
			janitor := &ccbridge.Janitor{
				Root:     seederRoot,
				Checker:  alwaysInactiveChecker{},
				Grace:    janitorGrace,
				Interval: janitorInterval,
			}

			deps := sidecar.Deps{
				Logger:           zerolog.New(cmd.ErrOrStderr()).With().Timestamp().Logger(),
				Signer:           signer,
				AuditLog:         al,
				CCProxyAddr:      defaultCCProxyAddr,
				TrackerEndpoints: endpoints,
				Janitor:          janitor,
			}

			app, err := sidecar.New(deps)
			if err != nil {
				_ = al.Close()
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return app.Run(ctx)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to ~/.token-bay/config.yaml (required)")
	return cmd
}

// resolveTrackerEndpoints turns cfg.Tracker into a one-element bootstrap
// list. v1 only supports an explicit URL — `auto` is rejected because no
// resolver exists yet.
func resolveTrackerEndpoints(spec string) ([]trackerclient.TrackerEndpoint, error) {
	if spec == "auto" {
		return nil, errors.New("tracker: auto-bootstrap not yet implemented; configure an explicit tracker URL")
	}
	u, err := url.Parse(spec)
	if err != nil {
		return nil, fmt.Errorf("parse tracker URL %q: %w", spec, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("tracker URL %q has no host", spec)
	}

	hashHex := strings.TrimSpace(os.Getenv(envTrackerHash))
	if hashHex == "" {
		return nil, fmt.Errorf("%s env var must be set to the hex SHA-256 of the tracker's Ed25519 SPKI", envTrackerHash)
	}
	raw, err := hex.DecodeString(hashHex)
	if err != nil || len(raw) != sha256.Size {
		return nil, fmt.Errorf("%s must be %d hex bytes", envTrackerHash, sha256.Size)
	}
	var hash [32]byte
	copy(hash[:], raw)

	return []trackerclient.TrackerEndpoint{{
		Addr:         u.Host,
		IdentityHash: hash,
		Region:       "configured",
	}}, nil
}

// alwaysInactiveChecker reports every peer as inactive. Used as the
// initial ccbridge.Janitor checker until a peer-tracker subsystem
// lands and can supply real liveness state. With this stub, per-
// client folders are reaped purely on the grace-window mtime check —
// a folder fresher than janitorGrace survives, anything older gets
// removed. Conservative: an active peer that goes idle for grace
// loses its state, but the cost is one re-uploaded history on next
// request.
type alwaysInactiveChecker struct{}

func (alwaysInactiveChecker) IsActive(_ ed25519.PublicKey) bool { return false }
func (alwaysInactiveChecker) IsActiveByHash(_ string) bool      { return false }

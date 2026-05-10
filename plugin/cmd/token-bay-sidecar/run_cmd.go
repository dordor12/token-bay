package main

import (
	"context"
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
	"github.com/token-bay/token-bay/plugin/internal/seederflow"
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

			logger := zerolog.New(cmd.ErrOrStderr()).With().Timestamp().Logger()

			seederRoot := filepath.Join(cfgDir, "seeder-sessions")
			runner := &ccbridge.ExecRunner{
				BinaryPath: cfg.CCBridge.ClaudeBin,
				SeederRoot: seederRoot,
			}

			// Build the seeder coordinator first with a placeholder
			// UsageReporter so its required-field check passes; the
			// trackerclient is constructed next with the coordinator
			// wired as OfferHandler, and we late-bind it via
			// SetTracker before Run.
			coord, err := buildSeederFlow(cfg, al, signer, runner, logger)
			if err != nil {
				_ = al.Close()
				return fmt.Errorf("build seederflow: %w", err)
			}

			tracker, err := trackerclient.New(trackerclient.Config{
				Endpoints:    endpoints,
				Identity:     signer,
				Logger:       logger,
				OfferHandler: coord,
			})
			if err != nil {
				_ = al.Close()
				return fmt.Errorf("build trackerclient: %w", err)
			}
			coord.SetTracker(tracker)

			janitor := &ccbridge.Janitor{
				Root:     seederRoot,
				Checker:  coord,
				Grace:    janitorGrace,
				Interval: janitorInterval,
			}

			deps := sidecar.Deps{
				Logger:           logger,
				Signer:           signer,
				AuditLog:         al,
				CCProxyAddr:      defaultCCProxyAddr,
				TrackerEndpoints: endpoints,
				TrackerClient:    tracker,
				Janitor:          janitor,
				SeederFlow:       coord,
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

// buildSeederFlow constructs the seederflow.Coordinator from cfg with
// every dependency wired. The Acceptor is the v1 NopAcceptor — see
// seederflow/doc.go for the wire-format gap that prevents binding a
// real tunnel.Listener at this layer.
func buildSeederFlow(
	cfg *config.Config,
	al *auditlog.Logger,
	signer *identity.Signer,
	runner ccbridge.Runner,
	logger zerolog.Logger,
) (*seederflow.Coordinator, error) {
	idle, err := seederflow.ParseIdlePolicy(cfg.IdlePolicy.Mode, cfg.IdlePolicy.Window)
	if err != nil {
		return nil, err
	}
	models := []string{"claude-sonnet-4-6", "claude-opus-4-7"}
	scfg := seederflow.Config{
		Logger:         logger,
		Bridge:         ccbridge.NewBridge(runner),
		AuditLog:       al,
		Signer:         signer,
		Acceptor:       seederflow.NopAcceptor{},
		Runner:         runner,
		ConformanceFn:  ccbridge.RunStartupConformance,
		IdlePolicy:     idle,
		ActivityGrace:  cfg.IdlePolicy.ActivityGrace.AsDuration(),
		HeadroomWindow: cfg.Seeder.HeadroomWindow.AsDuration(),
		Models:         models,
	}
	// Tracker is late-bound via SetTracker once the final
	// trackerclient.Client is built with this coordinator wired as
	// OfferHandler. Use a placeholder so seederflow.New's required-
	// field check passes — the placeholder is replaced before Run.
	scfg.Tracker = &deferredTracker{}
	return seederflow.New(scfg)
}

// deferredTracker is a placeholder UsageReporter handed to seederflow
// during construction so the Tracker required-field check passes; the
// cmd layer replaces it via Coordinator.SetTracker once the final
// trackerclient.Client is wired with the coordinator as OfferHandler.
// Calling its methods before SetTracker indicates a wiring bug.
type deferredTracker struct{}

func (deferredTracker) UsageReport(_ context.Context, _ *trackerclient.UsageReport) error {
	return errors.New("deferredTracker: SetTracker was not called before use")
}

func (deferredTracker) Advertise(_ context.Context, _ *trackerclient.Advertisement) error {
	return errors.New("deferredTracker: SetTracker was not called before use")
}

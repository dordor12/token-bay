package main

import (
	"context"
	"errors"
	"time"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/federation"
	"github.com/token-bay/token-bay/tracker/internal/registry"
	"github.com/token-bay/token-bay/tracker/internal/stunturn"
)

// registrySweepInterval is how often the stale-seeder reaper runs. Short
// enough that dead heartbeats are gc'd well within an offer-loop attempt
// window, large enough to not noticeably contend with Heartbeat writes.
const registrySweepInterval = 30 * time.Second

// stunturnSweepInterval matches the cadence the allocator doc recommends
// ("intended to run from a single goroutine on a periodic timer (e.g.,
// every 1s)"). Allocator.Sweep is O(active sessions) and lock-light.
const stunturnSweepInterval = time.Second

// startMaintenanceLoops spawns the periodic background routines that the
// composition root owns but no subsystem owns internally: federation root
// publisher, registry stale-seeder sweep, and STUN/TURN session sweep.
// All exit on ctx cancel.
func startMaintenanceLoops(
	ctx context.Context,
	logger zerolog.Logger,
	fed *federation.Federation,
	reg *registry.Registry,
	alloc *stunturn.Allocator,
	cfg *config.Config,
) {
	startFederationPublisher(ctx, logger, fed, cfg)
	startRegistrySweeper(ctx, logger, reg, cfg)
	startStunturnSweeper(ctx, alloc)
}

// startFederationPublisher ticks at cfg.Federation.PublishCadenceS and
// drives both Federation.PublishHour (root-attestation for the most
// recently complete hour) and Federation.PublishPeerExchange (known-peer
// gossip). Per federation peer-exchange design §"out of scope", v1
// reuses one cadence for both publishers; splitting into separate knobs
// is mechanical when operators need it.
//
// PublishHour silently no-ops when the ledger has not yet produced a
// MerkleRoot for that hour (ReadyRoot returns ok=false), so this is safe
// to enable even before the ledger Merkle orchestrator lands.
// PublishPeerExchange returns ErrPeerExchangeDisabled when no
// KnownPeersArchive Dep was supplied; that is a normal "feature not
// configured" signal, not an error condition.
func startFederationPublisher(
	ctx context.Context,
	logger zerolog.Logger,
	fed *federation.Federation,
	cfg *config.Config,
) {
	cadence := time.Duration(cfg.Federation.PublishCadenceS) * time.Second
	if cadence <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(cadence)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				hour := uint64(now.Unix()) / 3600 //nolint:gosec // G115 — post-1970
				if hour > 0 {
					if err := fed.PublishHour(ctx, hour-1); err != nil {
						logger.Warn().Err(err).Uint64("hour", hour-1).Msg("federation publisher: PublishHour")
					}
				}
				if err := fed.PublishPeerExchange(ctx); err != nil &&
					!errors.Is(err, federation.ErrPeerExchangeDisabled) {
					logger.Warn().Err(err).Msg("federation publisher: PublishPeerExchange")
				}
			}
		}
	}()
}

// startRegistrySweeper periodically calls Registry.Sweep to evict seeders
// whose LastHeartbeat falls outside the admission HeartbeatWindow. Without
// this loop, dead seeder records accumulate forever and broker selection
// progressively wastes attempts on unreachable peers.
func startRegistrySweeper(
	ctx context.Context,
	logger zerolog.Logger,
	reg *registry.Registry,
	cfg *config.Config,
) {
	window := time.Duration(cfg.Admission.HeartbeatWindowMinutes) * time.Minute
	if window <= 0 {
		window = 10 * time.Minute
	}
	go func() {
		t := time.NewTicker(registrySweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				removed := reg.Sweep(now.Add(-window))
				if removed > 0 {
					logger.Debug().Int("removed", removed).Msg("registry: stale seeder sweep")
				}
			}
		}
	}()
}

// startStunturnSweeper periodically calls Allocator.Sweep to release
// sessions whose LastActive exceeds SessionTTL. Without this loop,
// expired TURN sessions accumulate and the bandwidth-cap accounting
// becomes inaccurate over a long-running tracker.
func startStunturnSweeper(ctx context.Context, alloc *stunturn.Allocator) {
	go func() {
		t := time.NewTicker(stunturnSweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				alloc.Sweep(now)
			}
		}
	}()
}

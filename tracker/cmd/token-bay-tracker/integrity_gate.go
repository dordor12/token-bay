package main

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/tracker/internal/ledger"
)

// runStartupIntegrityCheck walks the local ledger chain before any
// server starts. Spec §8 acceptance: hash(entry[n-1]) == entry[n].prev_hash
// for all n. A corrupt chain must NOT serve traffic, so the caller is
// expected to fail startup non-zero on a non-nil return.
//
// On success the gate emits an info-level structured event ("ledger
// integrity verified at startup") so operators can confirm it ran. The
// result counter is bumped on both paths.
func runStartupIntegrityCheck(
	ctx context.Context,
	led *ledger.Ledger,
	m *ledger.IntegrityMetrics,
	logger zerolog.Logger,
) error {
	if err := led.AssertChainIntegrity(ctx, 0, 0); err != nil {
		m.RecordResult("fail")
		logger.Error().Err(err).Str("gate", "startup").Msg("ledger integrity check failed")
		return fmt.Errorf("ledger integrity check failed at startup: %w", err)
	}
	m.RecordResult("pass")
	logger.Info().Str("gate", "startup").Msg("ledger integrity verified at startup")
	return nil
}

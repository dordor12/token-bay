package main

import (
	"context"
	"encoding/hex"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/federation"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
)

// depeerFunc abstracts federation.Federation.Depeer so the reconnect
// gate is testable without spinning up the full subsystem.
type depeerFunc func(id ids.TrackerID, reason federation.DepeerReason) error

// runReconnectIntegrityCheck is the per-peer-reconnect gate. Spec §8
// acceptance: on every peer reconnect, walk the local chain and confirm
// hash(entry[n-1]) == entry[n].prev_hash, plus each entry's recomputed
// body hash equals its append-time stored hash (the check that covers
// the tip). A break means the local store is corrupt — drop the peer
// that just attached (we do not want to feed possibly-incorrect roots
// upstream) and record the failure.
//
// Called by the federation subsystem in its own goroutine, so it must
// not assume any caller-side serialization.
func runReconnectIntegrityCheck(
	ctx context.Context,
	led *ledger.Ledger,
	m *ledger.IntegrityMetrics,
	logger zerolog.Logger,
	peer ids.TrackerID,
	depeer depeerFunc,
) {
	peerBytes := peer.Bytes()
	peerHex := hex.EncodeToString(peerBytes[:8])
	if err := led.AssertChainIntegrity(ctx, 0, 0); err != nil {
		m.RecordResult("fail")
		logger.Error().Err(err).Str("gate", "reconnect").Str("peer", peerHex).
			Msg("ledger integrity check failed on peer reconnect; dropping peer")
		if depeer != nil {
			if derr := depeer(peer, federation.ReasonLocalChainCorrupt); derr != nil {
				logger.Warn().Err(derr).Str("peer", peerHex).
					Msg("ledger integrity gate: depeer failed")
			}
		}
		return
	}
	m.RecordResult("pass")
	logger.Info().Str("gate", "reconnect").Str("peer", peerHex).
		Msg("ledger integrity verified on peer reconnect")
}

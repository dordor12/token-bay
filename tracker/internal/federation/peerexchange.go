package federation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"time"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
	"google.golang.org/protobuf/proto"
)

const defaultPeerExchangeEmitCap = 256

// peerExchangeCoordinator implements §7.1 PEER_EXCHANGE: hourly-cadence
// (on-demand in v1) gossip of a KnownPeer table, plus inbound merge.
//
// Outbound: EmitNow(ctx) lists the top-EmitCap rows from the archive,
// computes a fresh health_score per row via *PeerHealth (slice 5),
// writes the freshly-computed score back to the archive row, packs
// into a PeerExchange proto, marshals, and Forwards.
//
// Inbound: OnIncoming(ctx, env, msg) upserts every entry (skipping
// self-entries by tracker_id) and forwards the envelope onward via the
// slice-0 dedupe-and-forward gossip core.
type peerExchangeCoordinator struct {
	cfg peerExchangeCoordinatorCfg
}

type peerExchangeCoordinatorCfg struct {
	MyTrackerID ids.TrackerID
	MyPriv      ed25519.PrivateKey
	Archive     KnownPeersArchive
	Forward     Forwarder
	Health      *PeerHealth
	Now         func() time.Time
	EmitCap     int
	Invalid     func(reason string)
	OnEmit      func()
	OnReceived  func(outcome string)
}

func newPeerExchangeCoordinator(cfg peerExchangeCoordinatorCfg) *peerExchangeCoordinator {
	if cfg.EmitCap <= 0 {
		cfg.EmitCap = defaultPeerExchangeEmitCap
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &peerExchangeCoordinator{cfg: cfg}
}

// EmitNow snapshots the archive and broadcasts a PeerExchange envelope
// to all active peers via Forward. Returns the archive's error verbatim
// if the snapshot fails; otherwise nil.
func (pc *peerExchangeCoordinator) EmitNow(ctx context.Context) error {
	rows, err := pc.cfg.Archive.ListKnownPeers(ctx, pc.cfg.EmitCap, true)
	if err != nil {
		return err
	}

	out := make([]*fed.KnownPeer, 0, len(rows))
	for _, r := range rows {
		var tid ids.TrackerID
		copy(tid[:], r.TrackerID)
		score := pc.cfg.Health.Score(tid, pc.cfg.Now())
		if err := pc.cfg.Archive.UpdateKnownPeerHealth(ctx, r.TrackerID, score); err != nil {
			pc.cfg.Invalid("health_persist")
		}
		out = append(out, &fed.KnownPeer{
			TrackerId:   append([]byte(nil), r.TrackerID...),
			Addr:        r.Addr,
			LastSeen:    uint64(r.LastSeen.Unix()), //nolint:gosec // G115 — Unix() is non-negative for any sane wall clock
			RegionHint:  r.RegionHint,
			HealthScore: score,
		})
	}

	payload, err := proto.Marshal(&fed.PeerExchange{Peers: out})
	if err != nil {
		return err
	}
	pc.cfg.Forward(ctx, fed.Kind_KIND_PEER_EXCHANGE, payload)
	pc.cfg.OnEmit()
	return nil
}

// OnIncoming merges a received PeerExchange into the archive (skipping
// self-entries) and forwards the envelope onward.
func (pc *peerExchangeCoordinator) OnIncoming(ctx context.Context, env *fed.Envelope, msg *fed.PeerExchange) {
	myBytes := pc.cfg.MyTrackerID[:]
	for _, p := range msg.Peers {
		if bytes.Equal(p.TrackerId, myBytes) {
			continue
		}
		_ = pc.cfg.Archive.UpsertKnownPeer(ctx, storage.KnownPeer{
			TrackerID:   append([]byte(nil), p.TrackerId...),
			Addr:        p.Addr,
			LastSeen:    time.Unix(int64(p.LastSeen), 0), //nolint:gosec // G115 — validator already capped above
			RegionHint:  p.RegionHint,
			HealthScore: p.HealthScore,
			Source:      "gossip",
		})
	}
	pc.cfg.Forward(ctx, fed.Kind_KIND_PEER_EXCHANGE, env.Payload)
	pc.cfg.OnReceived("merged")
}

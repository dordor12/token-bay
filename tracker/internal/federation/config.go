package federation

import (
	"context"
	"crypto/ed25519"
	"time"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/shared/ids"
)

// AllowlistedPeer is one operator-allowlisted peer. It is the config-time
// representation; the live recv-loop counterpart is the unexported *Peer
// type in peer.go.
type AllowlistedPeer struct {
	TrackerID ids.TrackerID
	PubKey    ed25519.PublicKey
	Addr      string
	Region    string
}

// Config is the subsystem's runtime config. Fields with sensible defaults
// can be left zero; Open fills them in.
type Config struct {
	MyTrackerID      ids.TrackerID
	MyPriv           ed25519.PrivateKey
	HandshakeTimeout time.Duration // default 5s
	DedupeTTL        time.Duration // default 1h
	DedupeCap        int           // default 64*1024
	GossipRateQPS    int           // default 100 (informational; Forward is best-effort)
	SendQueueDepth   int           // default 256
	PublishCadence   time.Duration // default 1h
	IdleTimeout      time.Duration // default 60s; passed to QUIC MaxIdleTimeout
	RedialBase       time.Duration // default 1s; per-peer redial initial backoff
	RedialMax        time.Duration // default 30s; per-peer redial backoff cap
	TransferTimeout  time.Duration // default 30s; cross-region StartTransfer wait
	IssuedProofCap   int           // default 4096; source-side replay cache LRU cap
	Peers            []AllowlistedPeer
	Health           HealthConfig

	// PeerExchangeCadence drives the slice-7 periodic emit goroutine.
	// Zero or negative disables the ticker entirely (operators must
	// call Federation.PublishPeerExchange explicitly). Default 1h.
	PeerExchangeCadence time.Duration

	// RateLimit caps inbound gossip per (peer, kind). Slice 9. Zero
	// rate per bucket disables that bucket.
	RateLimit RateLimitConfig

	// KnownPeersPruneInterval drives the slice-10 auto-pruner. Zero
	// disables. Default 1h.
	KnownPeersPruneInterval time.Duration

	// KnownPeersMaxAge is the cutoff for gossip-sourced rows
	// (last_seen < now - MaxAge → deleted by pruner). Default 7d.
	KnownPeersMaxAge time.Duration

	// LowHealthThreshold + LowHealthSustainedWindow drive slice-11
	// automatic depeer. 0 in either disables. Default 0.2 + 30m.
	LowHealthThreshold       float64
	LowHealthSustainedWindow time.Duration
	HealthWatchInterval      time.Duration // default 5m

	// IntegrityCheckOnReconnect gates the post-handshake ledger-chain
	// integrity audit (spec §8). nil means "use default" (true); a
	// non-nil pointer makes the operator's choice explicit. Hook
	// implementations are wired via Deps.OnPeerReconnect — leaving that
	// nil silently disables the gate regardless of this knob.
	IntegrityCheckOnReconnect *bool
}

// Deps is the wired-in collaborators (Transport, RootSource, archive,
// metrics, logger, clock).
type Deps struct {
	Transport Transport
	RootSrc   RootSource
	Archive   PeerRootArchive
	Metrics   *Metrics
	Logger    zerolog.Logger
	Now       func() time.Time

	// Ledger is the cross-region credit transfer hook. May be nil; when
	// nil, Federation.StartTransfer returns ErrTransferDisabled and
	// inbound transfer kinds are rejected with the same.
	Ledger LedgerHooks

	// RevocationArchive is the federation→storage hook for peer
	// revocations. May be nil; when nil, Federation.OnFreeze is a
	// no-op and inbound KIND_REVOCATION is rejected with metric
	// reason "revocation_disabled".
	RevocationArchive PeerRevocationArchive

	// KnownPeers is the federation→storage hook for peer-exchange
	// (slice 3). May be nil; when nil, Federation.PublishPeerExchange
	// returns ErrPeerExchangeDisabled and inbound KIND_PEER_EXCHANGE is
	// rejected with metric reason "peer_exchange_disabled".
	KnownPeers KnownPeersArchive

	// OnPeerReconnect, when non-nil, is invoked asynchronously after
	// every successful peer attach (steady transition). The composition
	// root uses it to run the spec §8 ledger-chain integrity check and
	// drop the peer on local-chain corruption. Gated by
	// Config.IntegrityCheckOnReconnect (default true). Hooks must
	// tolerate Federation.Close cancelling the supplied context.
	OnPeerReconnect func(ctx context.Context, peer ids.TrackerID)
}

func (c Config) withDefaults() Config {
	if c.HandshakeTimeout == 0 {
		c.HandshakeTimeout = 5 * time.Second
	}
	if c.DedupeTTL == 0 {
		c.DedupeTTL = time.Hour
	}
	if c.DedupeCap == 0 {
		c.DedupeCap = 64 * 1024
	}
	if c.GossipRateQPS == 0 {
		c.GossipRateQPS = 100
	}
	if c.SendQueueDepth == 0 {
		c.SendQueueDepth = 256
	}
	if c.PublishCadence == 0 {
		c.PublishCadence = time.Hour
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = 60 * time.Second
	}
	if c.RedialBase == 0 {
		c.RedialBase = time.Second
	}
	if c.RedialMax == 0 {
		c.RedialMax = 30 * time.Second
	}
	if c.RedialMax < c.RedialBase {
		c.RedialMax = c.RedialBase
	}
	if c.TransferTimeout == 0 {
		c.TransferTimeout = 30 * time.Second
	}
	if c.IssuedProofCap == 0 {
		c.IssuedProofCap = 4096
	}
	if c.PeerExchangeCadence == 0 {
		c.PeerExchangeCadence = time.Hour
	}
	if c.KnownPeersPruneInterval == 0 {
		c.KnownPeersPruneInterval = time.Hour
	}
	if c.KnownPeersMaxAge == 0 {
		c.KnownPeersMaxAge = 7 * 24 * time.Hour
	}
	if c.LowHealthThreshold == 0 {
		c.LowHealthThreshold = 0.2
	}
	if c.LowHealthSustainedWindow == 0 {
		c.LowHealthSustainedWindow = 30 * time.Minute
	}
	if c.HealthWatchInterval == 0 {
		c.HealthWatchInterval = 5 * time.Minute
	}
	return c
}

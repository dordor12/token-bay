package federation

import (
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
	return c
}

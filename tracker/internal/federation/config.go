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
	Peers            []AllowlistedPeer
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
	return c
}

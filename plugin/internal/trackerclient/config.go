package trackerclient

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/rs/zerolog"
)

// OfferHandler is the seeder-side hook for server-pushed offers.
type OfferHandler interface {
	HandleOffer(ctx Ctx, o *Offer) (OfferDecision, error)
}

// SettlementHandler is the consumer-side hook for server-pushed settlement
// requests. Returns the consumer's signature over the preimage; an error
// causes the supervisor to reject the settlement back to the tracker.
type SettlementHandler interface {
	HandleSettlement(ctx Ctx, r *SettlementRequest) (sig []byte, err error)
}

// PeerExchangeHandler is the plugin-side hook for inbound federation
// peer-exchange pushes (federation §7.1). Implementations typically
// merge the entries into the local known-peers cache. Errors are
// logged by the dispatcher and otherwise ignored — peer-exchange is a
// fire-and-forget gossip channel, not RPC.
type PeerExchangeHandler interface {
	HandlePeerExchange(ctx Ctx, peers []BootstrapPeer) error
}

// Ctx is an alias to keep handler signatures stable if we later swap to a
// purpose-built request context.
type Ctx = interface {
	Done() <-chan struct{}
	Err() error
}

// Config configures Client. Required fields: Endpoints, Identity. If
// the consumer role is in use, SettlementHandler is required. If the
// seeder role is in use, OfferHandler is required.
type Config struct {
	Endpoints []TrackerEndpoint
	Identity  Signer
	Transport Transport // optional; defaults to QUIC

	OfferHandler        OfferHandler
	SettlementHandler   SettlementHandler
	PeerExchangeHandler PeerExchangeHandler

	Logger zerolog.Logger
	Clock  func() time.Time
	Rand   io.Reader

	DialTimeout            time.Duration
	HeartbeatPeriod        time.Duration
	HeartbeatMisses        int
	BalanceTTL             time.Duration
	BalanceRefreshHeadroom time.Duration

	BackoffBase  time.Duration
	BackoffMax   time.Duration
	MaxFrameSize int

	// Metrics is an optional observability hook. Currently used only by
	// FetchBootstrapPeers; future RPCs may attach their own counters.
	Metrics BootstrapMetrics
}

// BootstrapMetrics is the optional observability hook for bootstrap-list
// fetches. Nil-safe — Client skips metrics if absent.
type BootstrapMetrics interface {
	IncBootstrapPeersFetched(outcome string) // outcome ∈ {ok, sig_invalid, expired, invalid, empty, issuer_mismatch, rpc_error}
}

// withDefaults returns a copy of cfg with zero-valued fields filled in.
func (cfg Config) withDefaults() Config {
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 5 * time.Second
	}
	if cfg.HeartbeatPeriod == 0 {
		cfg.HeartbeatPeriod = 15 * time.Second
	}
	if cfg.HeartbeatMisses == 0 {
		cfg.HeartbeatMisses = 3
	}
	if cfg.BalanceTTL == 0 {
		cfg.BalanceTTL = 10 * time.Minute
	}
	if cfg.BalanceRefreshHeadroom == 0 {
		cfg.BalanceRefreshHeadroom = 2 * time.Minute
	}
	if cfg.BackoffBase == 0 {
		cfg.BackoffBase = 200 * time.Millisecond
	}
	if cfg.BackoffMax == 0 {
		cfg.BackoffMax = 30 * time.Second
	}
	if cfg.MaxFrameSize == 0 {
		cfg.MaxFrameSize = 1 << 20
	}
	return cfg
}

// Validate enforces required-field and range invariants.
func (cfg Config) Validate() error {
	if len(cfg.Endpoints) == 0 {
		return fmt.Errorf("%w: at least one endpoint required", ErrInvalidEndpoint)
	}
	for i, ep := range cfg.Endpoints {
		if ep.Addr == "" {
			return fmt.Errorf("%w: endpoints[%d].Addr empty", ErrInvalidEndpoint, i)
		}
		if ep.IdentityHash == ([32]byte{}) {
			return fmt.Errorf("%w: endpoints[%d].IdentityHash zero", ErrInvalidEndpoint, i)
		}
	}
	if cfg.Identity == nil {
		return fmt.Errorf("%w: Identity required", ErrConfigInvalid)
	}
	if cfg.HeartbeatMisses < 1 {
		return fmt.Errorf("%w: HeartbeatMisses must be ≥ 1", ErrConfigInvalid)
	}
	if cfg.BalanceRefreshHeadroom >= cfg.BalanceTTL {
		return fmt.Errorf("%w: BalanceRefreshHeadroom must be < BalanceTTL", ErrConfigInvalid)
	}
	if cfg.MaxFrameSize <= 0 {
		return fmt.Errorf("%w: MaxFrameSize must be > 0", ErrConfigInvalid)
	}
	if cfg.BackoffBase <= 0 || cfg.BackoffMax <= 0 || cfg.BackoffMax < cfg.BackoffBase {
		return fmt.Errorf("%w: bad backoff window", ErrConfigInvalid)
	}
	return nil
}

// errMustOptional is a sentinel used internally to signal that a config
// field is conditionally required at Start() time, not at Validate().
//
//nolint:unused // reserved for future Start-time conditional checks
var errMustOptional = errors.New("trackerclient: optional")

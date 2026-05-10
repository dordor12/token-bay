package consumerflow

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"time"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/plugin/internal/auditlog"
	"github.com/token-bay/token-bay/plugin/internal/ccproxy"
	"github.com/token-bay/token-bay/plugin/internal/envelopebuilder"
	"github.com/token-bay/token-bay/plugin/internal/exhaustionproofbuilder"
	"github.com/token-bay/token-bay/plugin/internal/ratelimit"
	"github.com/token-bay/token-bay/plugin/internal/settingsjson"
	"github.com/token-bay/token-bay/shared/exhaustionproof"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// BrokerClient is the trackerclient subset Coordinator needs. The
// production *trackerclient.Client satisfies this directly.
type BrokerClient interface {
	BrokerRequest(ctx context.Context, env *tbproto.EnvelopeSigned) (*BrokerResult, error)
	BalanceCached(ctx context.Context, id ids.IdentityID) (*tbproto.SignedBalanceSnapshot, error)
	Settle(ctx context.Context, preimageHash, sig []byte) error
}

// BrokerResult is the consumerflow-local mirror of trackerclient.BrokerResult.
// We keep a dedicated type so this package doesn't depend on trackerclient
// for type identity — the cmd layer adapts trackerclient's BrokerResult into
// this shape via a small adapter (kept in the cmd layer to avoid a circular
// import potential).
type BrokerResult struct {
	Outcome    BrokerOutcome
	Assignment *SeederAssignment
	NoCap      *NoCapacityResult
	Queued     *QueuedResult
	Rejected   *RejectedResult
}

// BrokerOutcome enumerates BrokerRequest outcomes.
type BrokerOutcome int

// BrokerOutcome values mirror trackerclient.BrokerOutcome.
const (
	BrokerOutcomeUnspecified BrokerOutcome = iota
	BrokerOutcomeAssignment
	BrokerOutcomeNoCapacity
	BrokerOutcomeQueued
	BrokerOutcomeRejected
)

// SeederAssignment carries the routing data for the consumer-seeder tunnel.
type SeederAssignment struct {
	SeederAddr       string
	SeederPubkey     []byte
	ReservationToken []byte
}

// NoCapacityResult mirrors trackerclient.NoCapacityResult.
type NoCapacityResult struct {
	Reason string
}

// QueuedResult mirrors trackerclient.QueuedResult.
type QueuedResult struct {
	RequestID    [16]byte
	PositionBand uint8
	EtaBand      uint8
}

// RejectedResult mirrors trackerclient.RejectedResult.
type RejectedResult struct {
	Reason      uint8
	RetryAfterS uint32
}

// SessionStoreWriter is the ccproxy.SessionModeStore subset Coordinator
// uses. *ccproxy.SessionModeStore satisfies this directly.
type SessionStoreWriter interface {
	EnterNetworkMode(sessionID string, meta ccproxy.EntryMetadata)
	ExitNetworkMode(sessionID string) bool
}

// SettingsStoreWriter is the settingsjson.Store subset Coordinator uses.
// *settingsjson.Store satisfies this directly.
type SettingsStoreWriter interface {
	GetState(sidecarURL string) (*settingsjson.State, error)
	EnterNetworkMode(sidecarURL, sessionID string) error
	ExitNetworkMode() error
	ExitNetworkModeBestEffort(sidecarURL string) error
}

// AuditWriter is the auditlog.Logger subset Coordinator uses.
type AuditWriter interface {
	LogConsumer(rec auditlog.ConsumerRecord) error
}

// ProofBuilder is the exhaustionproofbuilder.Builder subset Coordinator uses.
type ProofBuilder interface {
	Build(in exhaustionproofbuilder.ProofInput) (*exhaustionproof.ExhaustionProofV1, error)
}

// EnvelopeBuilder is the envelopebuilder.Builder subset Coordinator uses.
type EnvelopeBuilder interface {
	Build(spec envelopebuilder.RequestSpec, proof *exhaustionproof.ExhaustionProofV1, balance *tbproto.SignedBalanceSnapshot) (*tbproto.EnvelopeSigned, error)
}

// IdentityProvider exposes the consumer's IdentityID for balance lookup
// and signs preimage bodies as part of settlement countersign. The
// production *identity.Signer satisfies this directly.
type IdentityProvider interface {
	IdentityID() ids.IdentityID
	Sign(msg []byte) ([]byte, error)
}

// SettlementMetrics is the optional observability hook for the consumer's
// countersign decisions. Nil-safe — Coordinator skips metrics if absent.
type SettlementMetrics interface {
	IncSettlementDecision(outcome string) // outcome ∈ {countersigned, refused_unknown_request, refused_over_budget, refused_preimage_mismatch, refused_decode_error, refused_sign_error, refused_settle_error}
}

// Deps is everything the Coordinator needs. The cmd layer constructs each
// field; this package never touches disk.
type Deps struct {
	// Logger is the structured logger. Required (zero value is acceptable).
	Logger zerolog.Logger

	// Tracker is the broker / balance client. Required.
	Tracker BrokerClient

	// Sessions is the per-session network-mode map shared with ccproxy.
	// Required.
	Sessions SessionStoreWriter

	// Settings is the settings.json mutator + rollback journal. Required.
	Settings SettingsStoreWriter

	// AuditLog is the append-only audit log. Required.
	AuditLog AuditWriter

	// UsageProber runs `claude /usage` under a PTY. Required.
	UsageProber ratelimit.ProbeRunner

	// ProofBuilder assembles ExhaustionProofV1 bundles. Required.
	ProofBuilder ProofBuilder

	// EnvelopeBuilder signs *EnvelopeSigned. Required.
	EnvelopeBuilder EnvelopeBuilder

	// Identity exposes the consumer's IdentityID for balance lookup.
	// Required.
	Identity IdentityProvider

	// SidecarURLFunc returns the resolved http://127.0.0.1:PORT/ sidecar
	// URL at use time. Lazy because ccproxy may bind to ":0" and the port
	// is only known after Start. Required; must return a non-empty URL.
	SidecarURLFunc func() string

	// NetworkModeTTL bounds how long a fallback session stays redirected.
	// 0 means no timer-based exit (only SessionEnd / explicit normal).
	// Defaults to 15 minutes per plugin spec §5.5 if zero.
	NetworkModeTTL time.Duration

	// RuntimeProbeOK reports whether the §5.3 runtime compatibility probe
	// has succeeded for this session. The cmd layer is responsible for
	// running the probe and passing the result. When false, OnStopFailure
	// refuses the fallback with a §11 diagnostic and audits the refusal.
	RuntimeProbeOK bool

	// PrivacyTier is the configured tier for envelope construction.
	// Defaults to PRIVACY_TIER_STANDARD if UNSPECIFIED.
	PrivacyTier tbproto.PrivacyTier

	// DefaultModel applied to envelopes assembled at hook time. v1 has no
	// model signal in the StopFailure payload (spec §10 open item) so the
	// cmd layer must supply one. Required.
	DefaultModel string

	// MaxInputTokens / MaxOutputTokens declare the envelope ceilings. v1
	// uses static config values rather than per-request introspection.
	// Both required and > 0.
	MaxInputTokens  uint64
	MaxOutputTokens uint64

	// EphemeralKeyGen returns a fresh Ed25519 keypair for the consumer
	// side of the seeder tunnel. Defaults to ed25519.GenerateKey(rand.Reader).
	EphemeralKeyGen func() (ed25519.PublicKey, ed25519.PrivateKey, error)

	// RandRead fills p with random bytes — used for the v1 placeholder
	// BodyHash since the request body is not available at hook time.
	// Defaults to crypto/rand.Read.
	RandRead func(p []byte) (n int, err error)

	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time

	// Metrics is an optional observability hook for settlement decisions.
	// Nil-safe.
	Metrics SettlementMetrics
}

// Validate enforces required-field invariants. Returns an ErrInvalidDeps chain.
func (d Deps) Validate() error {
	if d.Tracker == nil {
		return fmt.Errorf("%w: Tracker required", ErrInvalidDeps)
	}
	if d.Sessions == nil {
		return fmt.Errorf("%w: Sessions required", ErrInvalidDeps)
	}
	if d.Settings == nil {
		return fmt.Errorf("%w: Settings required", ErrInvalidDeps)
	}
	if d.AuditLog == nil {
		return fmt.Errorf("%w: AuditLog required", ErrInvalidDeps)
	}
	if d.UsageProber == nil {
		return fmt.Errorf("%w: UsageProber required", ErrInvalidDeps)
	}
	if d.ProofBuilder == nil {
		return fmt.Errorf("%w: ProofBuilder required", ErrInvalidDeps)
	}
	if d.EnvelopeBuilder == nil {
		return fmt.Errorf("%w: EnvelopeBuilder required", ErrInvalidDeps)
	}
	if d.Identity == nil {
		return fmt.Errorf("%w: Identity required", ErrInvalidDeps)
	}
	if d.SidecarURLFunc == nil {
		return fmt.Errorf("%w: SidecarURLFunc required", ErrInvalidDeps)
	}
	if d.DefaultModel == "" {
		return fmt.Errorf("%w: DefaultModel required", ErrInvalidDeps)
	}
	if d.MaxInputTokens == 0 {
		return fmt.Errorf("%w: MaxInputTokens must be > 0", ErrInvalidDeps)
	}
	if d.MaxOutputTokens == 0 {
		return fmt.Errorf("%w: MaxOutputTokens must be > 0", ErrInvalidDeps)
	}
	return nil
}

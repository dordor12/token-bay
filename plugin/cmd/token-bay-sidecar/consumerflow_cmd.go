package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/plugin/internal/auditlog"
	"github.com/token-bay/token-bay/plugin/internal/ccproxy"
	"github.com/token-bay/token-bay/plugin/internal/config"
	"github.com/token-bay/token-bay/plugin/internal/consumerflow"
	"github.com/token-bay/token-bay/plugin/internal/envelopebuilder"
	"github.com/token-bay/token-bay/plugin/internal/exhaustionproofbuilder"
	"github.com/token-bay/token-bay/plugin/internal/identity"
	"github.com/token-bay/token-bay/plugin/internal/ratelimit"
	"github.com/token-bay/token-bay/plugin/internal/settingsjson"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)

// v1 envelope ceilings used by consumerflow when assembling envelopes at
// hook time. The plugin config does not yet expose per-call ceilings (spec
// §10 open item — body is unavailable at hook time anyway), so static
// values are passed. Defaults track current Sonnet 4.6 / Opus 4.7 limits.
const (
	defaultConsumerModel   = "claude-sonnet-4-6"
	defaultMaxInputTokens  = 200_000
	defaultMaxOutputTokens = 8_192
)

// buildConsumerFlow wires the consumer-side fallback orchestrator with every
// collaborator it requires. Returns a Coordinator ready to be handed to
// sidecar.Deps.ConsumerFlow; the supervisor then runs its TTL-reap goroutine
// for the lifetime of ctx and the hooks subprocess (separate plan) delivers
// Sink events into it.
func buildConsumerFlow(
	cfg *config.Config,
	_ string, // cfgDir reserved for a future per-user rollback path; settings rollback currently lives under ~/.token-bay/.
	al *auditlog.Logger,
	signer *identity.Signer,
	tracker *trackerclient.Client,
	sessionStore *ccproxy.SessionModeStore,
	sidecarURLFunc func() string,
	logger zerolog.Logger,
) (*consumerflow.Coordinator, error) {
	settings, err := settingsjson.NewStore()
	if err != nil {
		return nil, fmt.Errorf("settingsjson: %w", err)
	}

	envBuilder := envelopebuilder.NewBuilder(envelopeBuilderSigner{signer: signer})
	proofBuilder := exhaustionproofbuilder.NewBuilder()
	probeRunner := ratelimit.NewClaudePTYProbeRunner()

	return consumerflow.New(consumerflow.Deps{
		Logger:          logger,
		Tracker:         consumerflowTracker{Client: tracker},
		Sessions:        sessionStore,
		Settings:        settings,
		AuditLog:        al,
		UsageProber:     probeRunner,
		ProofBuilder:    proofBuilder,
		EnvelopeBuilder: envBuilder,
		Identity:        signer,
		SidecarURLFunc:  sidecarURLFunc,
		NetworkModeTTL:  cfg.Consumer.NetworkModeTTL.AsDuration(),
		// RuntimeProbeOK gates whether OnStopFailure will attempt fallback
		// at all (spec §5.3). The actual runtime-compatibility probe lives
		// in a follow-on plan; until then we admit the path unconditionally
		// so the wiring exercises end-to-end.
		RuntimeProbeOK:  true,
		PrivacyTier:     parsePrivacyTier(cfg.PrivacyTier),
		DefaultModel:    defaultConsumerModel,
		MaxInputTokens:  defaultMaxInputTokens,
		MaxOutputTokens: defaultMaxOutputTokens,
	})
}

// parsePrivacyTier maps the YAML config string to the proto enum. Unknown
// values fall back to STANDARD (consumerflow itself normalises UNSPECIFIED
// to STANDARD, so this is just a fast-path for the common case).
func parsePrivacyTier(s string) tbproto.PrivacyTier {
	switch strings.ToLower(s) {
	case "tee":
		return tbproto.PrivacyTier_PRIVACY_TIER_TEE
	default:
		return tbproto.PrivacyTier_PRIVACY_TIER_STANDARD
	}
}

// envelopeBuilderSigner adapts identity.Signer (whose Sign takes raw bytes)
// to envelopebuilder.Signer (whose Sign takes *tbproto.EnvelopeBody and is
// expected to canonicalise + sign in one step). The canonicalisation lives
// in shared/signing.SignEnvelope, which is the cross-module source of
// truth — never reimplement the canonical form here.
type envelopeBuilderSigner struct {
	signer *identity.Signer
}

func (e envelopeBuilderSigner) Sign(body *tbproto.EnvelopeBody) ([]byte, error) {
	return signing.SignEnvelope(e.signer.PrivateKey(), body)
}

func (e envelopeBuilderSigner) IdentityID() ids.IdentityID {
	return e.signer.IdentityID()
}

// consumerflowTracker adapts *trackerclient.Client to the consumerflow's
// BrokerClient interface. The two packages keep mirrored result types
// (consumerflow/deps.go: "zero compile-time dependency on trackerclient's
// exported result types") so this adapter does a field-by-field copy
// across each oneof arm.
type consumerflowTracker struct {
	*trackerclient.Client
}

func (t consumerflowTracker) BrokerRequest(
	ctx context.Context,
	env *tbproto.EnvelopeSigned,
) (*consumerflow.BrokerResult, error) {
	r, err := t.Client.BrokerRequest(ctx, env)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, errors.New("consumerflowTracker: nil BrokerResult from trackerclient")
	}
	out := &consumerflow.BrokerResult{Outcome: consumerflow.BrokerOutcome(r.Outcome)}
	if r.Assignment != nil {
		out.Assignment = &consumerflow.SeederAssignment{
			SeederAddr:       r.Assignment.SeederAddr,
			SeederPubkey:     r.Assignment.SeederPubkey,
			ReservationToken: r.Assignment.ReservationToken,
		}
	}
	if r.NoCap != nil {
		out.NoCap = &consumerflow.NoCapacityResult{Reason: r.NoCap.Reason}
	}
	if r.Queued != nil {
		out.Queued = &consumerflow.QueuedResult{
			RequestID:    r.Queued.RequestID,
			PositionBand: r.Queued.PositionBand,
			EtaBand:      r.Queued.EtaBand,
		}
	}
	if r.Rejected != nil {
		out.Rejected = &consumerflow.RejectedResult{
			Reason:      r.Rejected.Reason,
			RetryAfterS: r.Rejected.RetryAfterS,
		}
	}
	return out, nil
}

// Package consumerflow is the consumer-side fallback orchestrator.
// It composes the leaf packages (hooks, ratelimit, exhaustionproofbuilder,
// envelopebuilder, settingsjson, ccproxy.SessionModeStore, trackerclient)
// into the §5 consumer flow defined in the plugin spec.
//
// Coordinator implements the hooks.Sink interface. When a StopFailure event
// with matcher=rate_limit fires, it runs the §5.2 two-stage verification,
// builds an ExhaustionProofV1 plus signed envelope, calls
// trackerclient.BrokerRequest, and on success enters network mode by
// (a) populating ccproxy.SessionModeStore with the seeder routing data and
// (b) atomically rewriting ~/.claude/settings.json via internal/settingsjson.
//
// Per plugin/internal/sidecar/CLAUDE.md, this package performs no
// filesystem I/O of its own — settingsjson.Store and auditlog.Logger are
// passed in via Deps. The cmd layer owns disk-resident state.
//
// Authoritative spec: docs/superpowers/specs/plugin/2026-04-22-plugin-design.md §5,
// failure-mode coverage in §11.
package consumerflow

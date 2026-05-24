package seederflow

import (
	"context"
	"crypto/ed25519"
	"io"
	"net/netip"
	"time"

	"github.com/token-bay/token-bay/plugin/internal/auditlog"
	"github.com/token-bay/token-bay/plugin/internal/ccbridge"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
)

// TunnelConn is the per-connection seeder-side surface. *tunnel.Tunnel
// satisfies this interface; tests provide a fake.
type TunnelConn interface {
	ReadRequest() ([]byte, error)
	SendOK() error
	ResponseWriter() io.Writer
	SendError(msg string) error
	CloseWrite() error
	Close() error
}

// TunnelAcceptor accepts inbound consumer-dialed QUIC tunnels. Production
// wraps a *tunnel.Listener; tests provide a fake.
//
// Bind installs the (seederPriv, consumerPub) pair the acceptor should use
// for the next inbound tunnel handshake. The seeder's private key is the
// freshly-generated per-offer ephemeral; the consumer's pubkey arrives via
// the OfferPush. Implementations may rebind their underlying tunnel
// listener with the new TLS pin material so QUIC handshakes from any
// other consumer fail closed. The Coordinator calls Bind exactly once
// per accepted offer, before returning the OfferDecision.
type TunnelAcceptor interface {
	Accept(ctx context.Context) (TunnelConn, error)
	LocalAddr() netip.AddrPort
	Bind(seederPriv ed25519.PrivateKey, consumerPub ed25519.PublicKey) error
	Close() error
}

// OfferMetrics is the optional observability hook for offer outcomes.
// Nil-safe — Coordinator skips metrics if absent.
type OfferMetrics interface {
	// IncOfferRejected bumps a counter for offers rejected before the
	// reservation is registered. reason is a stable low-cardinality slug:
	//   - "no_ephemeral"  consumer_ephemeral_pub absent or wrong length
	//   - "bind_failed"   acceptor.Bind returned an error
	IncOfferRejected(reason string)
}

// UsageReporter is the slice of trackerclient.Client used by the
// coordinator. *trackerclient.Client satisfies it.
type UsageReporter interface {
	UsageReport(ctx context.Context, ur *trackerclient.UsageReport) error
	Advertise(ctx context.Context, ad *trackerclient.Advertisement) error
}

// AuditSink is the slice of auditlog.Logger used by the coordinator.
// *auditlog.Logger satisfies it.
type AuditSink interface {
	LogSeeder(rec auditlog.SeederRecord) error
}

// Signer signs the seeder's per-request usage attestation. The plugin's
// identity.Signer satisfies it.
type Signer interface {
	Sign(msg []byte) ([]byte, error)
	PublicKey() ed25519.PublicKey
}

// IdleMode enumerates the seeder availability policies of plugin spec §6.3.
type IdleMode int

// IdleMode values.
const (
	// IdleAlwaysOn keeps the seeder advertised whenever activity grace
	// has elapsed and headroom is positive.
	IdleAlwaysOn IdleMode = iota
	// IdleScheduled gates advertising on a daily local-time window.
	IdleScheduled
)

// IdlePolicy is the parsed form of cfg.IdlePolicy.
type IdlePolicy struct {
	Mode IdleMode
	// WindowStart and WindowEnd are local-clock times-of-day with
	// only Hour/Minute/Second populated. Used only when Mode is
	// IdleScheduled. WindowStart > WindowEnd means a wrap-around
	// window (e.g. 22:00-06:00).
	WindowStart time.Time
	WindowEnd   time.Time
}

// Bridge is the seeder-side bridge surface the coordinator drives.
// *ccbridge.Bridge satisfies it.
type Bridge interface {
	Serve(ctx context.Context, req ccbridge.Request, sink io.Writer) (ccbridge.Usage, error)
}

// ConformanceFn runs the airtight-flag conformance check against a
// runner. *ccbridge.RunStartupConformance satisfies it.
type ConformanceFn func(ctx context.Context, runner ccbridge.Runner) error

package tunnel

import (
	"context"
	"fmt"
	"net/netip"
)

// Rendezvous is the orchestration seam between this package and the tracker
// client. The plugin's data-path coordinator implements it by adapting
// *trackerclient.Client; this package never imports trackerclient.
//
// Method semantics:
//
//   - AllocateReflexive reflects the caller's external address by invoking
//     the tracker's stun_allocate RPC. Returned address is the peer-visible
//     UDP endpoint of whatever socket the underlying transport observed the
//     RPC arrive from. Errors propagate verbatim.
//
//   - OpenRelay requests a TURN-style relay allocation for the given session
//     id (the brokered request id). On success the caller dials the returned
//     RelayCoords.Endpoint and authenticates with the same identity-pin as
//     the direct path; the Token is held by the caller for billing/accounting
//     correlation but is opaque to this package.
type Rendezvous interface {
	AllocateReflexive(ctx context.Context) (netip.AddrPort, error)
	OpenRelay(ctx context.Context, sessionID [16]byte) (RelayCoords, error)
}

// RelayCoords names a TURN-style relay endpoint and the opaque session token
// that authorizes its use. Endpoint is a UDP address the consumer dials with
// the same TLS-pinned handshake as the direct path; Token is held by the
// caller for billing correlation and is not transmitted on the data channel
// by this package.
type RelayCoords struct {
	Endpoint netip.AddrPort
	Token    []byte
}

// ParseRelayCoords converts the string-keyed wire form of a relay handle
// (as returned by trackerclient.RelayHandle{Endpoint, Token}) into a typed
// RelayCoords. The orchestration layer calls this immediately after
// trackerclient.TurnRelayOpen returns.
//
// Returns ErrInvalidConfig wrapping the parse error on an unparsable
// endpoint string.
func ParseRelayCoords(endpoint string, token []byte) (RelayCoords, error) {
	if endpoint == "" {
		return RelayCoords{}, fmt.Errorf("%w: empty relay endpoint", ErrInvalidConfig)
	}
	addr, err := netip.ParseAddrPort(endpoint)
	if err != nil {
		return RelayCoords{}, fmt.Errorf("%w: parse relay endpoint %q: %v", ErrInvalidConfig, endpoint, err)
	}
	return RelayCoords{Endpoint: addr, Token: token}, nil
}

// FormatRelayCoords renders a RelayCoords back to the string-keyed wire form
// suitable for trackerclient.RelayHandle. Inverse of ParseRelayCoords.
func FormatRelayCoords(rc RelayCoords) (endpoint string, token []byte) {
	return rc.Endpoint.String(), rc.Token
}

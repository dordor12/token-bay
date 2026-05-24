package trackerclient

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// bootstrapPeerListSkewToleranceS is the plugin-side clock-skew window
// applied when checking expires_at. A snapshot whose expires_at is up
// to 60 s in the past is still accepted.
const bootstrapPeerListSkewToleranceS = 60

// FetchBootstrapPeers calls RPC_METHOD_BOOTSTRAP_PEERS on the connected
// tracker, verifies the signature against the connection's peer
// pubkey, checks expires_at, and returns the parsed list. It does not
// persist the result — caller decides what to do with it.
func (c *Client) FetchBootstrapPeers(ctx context.Context) ([]BootstrapPeer, error) {
	peers, _, err := c.fetchBootstrapPeersWithExpiry(ctx)
	return peers, err
}

// fetchBootstrapPeersWithExpiry is the slice-6 variant that surfaces
// the signed expires_at so callers (the reroute supervisor) can cache
// the list and treat it as stale after the TTL. The two-method split
// keeps the public FetchBootstrapPeers signature stable.
func (c *Client) fetchBootstrapPeersWithExpiry(ctx context.Context) ([]BootstrapPeer, time.Time, error) {
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	pub := conn.PeerPublicKey()
	connID := conn.PeerIdentityID()

	var resp tbproto.BootstrapPeerList
	if err := c.callUnary(ctx, tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS,
		&tbproto.BootstrapPeersRequest{}, &resp); err != nil {
		c.observeBootstrapOutcome("rpc_error")
		return nil, time.Time{}, err
	}
	if err := tbproto.ValidateBootstrapPeerList(&resp); err != nil {
		c.observeBootstrapOutcome("invalid")
		return nil, time.Time{}, fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
	if !bytes.Equal(resp.IssuerId, connID[:]) {
		c.observeBootstrapOutcome("issuer_mismatch")
		return nil, time.Time{}, ErrBootstrapIssuerMismatch
	}
	if err := tbproto.VerifyBootstrapPeerListSig(pub, &resp); err != nil {
		c.observeBootstrapOutcome("sig_invalid")
		return nil, time.Time{}, err
	}
	now := c.cfg.Clock().Unix()
	if uint64(now) > resp.ExpiresAt+bootstrapPeerListSkewToleranceS { //nolint:gosec
		c.observeBootstrapOutcome("expired")
		return nil, time.Time{}, ErrBootstrapPeerListExpired
	}

	out := make([]BootstrapPeer, 0, len(resp.Peers))
	for _, p := range resp.Peers {
		var tid ids.IdentityID
		copy(tid[:], p.TrackerId)
		out = append(out, BootstrapPeer{
			TrackerID:   tid,
			Addr:        p.Addr,
			RegionHint:  p.RegionHint,
			HealthScore: p.HealthScore,
			LastSeen:    time.Unix(int64(p.LastSeen), 0), //nolint:gosec
		})
	}
	if len(out) == 0 {
		c.observeBootstrapOutcome("empty")
	} else {
		c.observeBootstrapOutcome("ok")
	}
	return out, time.Unix(int64(resp.ExpiresAt), 0), nil //nolint:gosec
}

// observeBootstrapOutcome emits to the optional metrics sink. Nil-safe.
func (c *Client) observeBootstrapOutcome(outcome string) {
	if c.cfg.Metrics != nil {
		c.cfg.Metrics.IncBootstrapPeersFetched(outcome)
	}
}

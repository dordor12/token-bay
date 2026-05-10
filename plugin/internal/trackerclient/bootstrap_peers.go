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
	conn, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	pub := conn.PeerPublicKey()
	connID := conn.PeerIdentityID()

	var resp tbproto.BootstrapPeerList
	if err := c.callUnary(ctx, tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS,
		&tbproto.BootstrapPeersRequest{}, &resp); err != nil {
		c.observeBootstrapOutcome("rpc_error")
		return nil, err
	}
	if err := tbproto.ValidateBootstrapPeerList(&resp); err != nil {
		c.observeBootstrapOutcome("invalid")
		return nil, fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
	if !bytes.Equal(resp.IssuerId, connID[:]) {
		c.observeBootstrapOutcome("issuer_mismatch")
		return nil, ErrBootstrapIssuerMismatch
	}
	if err := tbproto.VerifyBootstrapPeerListSig(pub, &resp); err != nil {
		c.observeBootstrapOutcome("sig_invalid")
		return nil, err
	}
	now := c.cfg.Clock().Unix()
	if uint64(now) > resp.ExpiresAt+bootstrapPeerListSkewToleranceS { //nolint:gosec
		c.observeBootstrapOutcome("expired")
		return nil, ErrBootstrapPeerListExpired
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
	return out, nil
}

// observeBootstrapOutcome emits to the optional metrics sink. Nil-safe.
func (c *Client) observeBootstrapOutcome(outcome string) {
	if c.cfg.Metrics != nil {
		c.cfg.Metrics.IncBootstrapPeersFetched(outcome)
	}
}

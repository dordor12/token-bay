package api

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

// BootstrapPeersService is the slice of dependencies the BOOTSTRAP_PEERS
// handler needs. The composition root in cmd/token-bay-tracker wires a
// small adapter that delegates ListKnownPeers to *storage.Store and
// IssuerID/Sign to closures over the tracker's identity Ed25519 key.
//
// MaxPeers is the per-call clamp; the handler over-fetches by 1 so the
// self-row drop still leaves MaxPeers entries. TTL is the snapshot
// lifetime.
type BootstrapPeersService interface {
	ListKnownPeers(ctx context.Context, limit int, byHealthDesc bool) ([]storage.KnownPeer, error)
	IssuerID() ids.IdentityID
	Sign(canonical []byte) ([]byte, error)
	MaxPeers() int
	TTL() time.Duration
}

func (r *Router) installBootstrapPeers() handlerFunc {
	if r.deps.BootstrapPeers == nil {
		return notImpl("bootstrap_peers")
	}
	svc := r.deps.BootstrapPeers
	return func(ctx context.Context, rc *RequestCtx, _ []byte) (*tbproto.RpcResponse, error) {
		max := svc.MaxPeers()
		if max <= 0 {
			max = 50
		}
		rows, err := svc.ListKnownPeers(ctx, max+1, true)
		if err != nil {
			return &tbproto.RpcResponse{
				Status: tbproto.RpcStatus_RPC_STATUS_INTERNAL,
				Error:  &tbproto.RpcError{Code: "BOOTSTRAP_LIST_STORAGE", Message: err.Error()},
			}, nil
		}

		issuer := svc.IssuerID()
		out := make([]*tbproto.BootstrapPeer, 0, len(rows))
		for _, row := range rows {
			if bytes.Equal(row.TrackerID, issuer[:]) {
				continue
			}
			out = append(out, &tbproto.BootstrapPeer{
				TrackerId:   row.TrackerID,
				Addr:        row.Addr,
				RegionHint:  row.RegionHint,
				HealthScore: row.HealthScore,
				LastSeen:    uint64(row.LastSeen.Unix()), //nolint:gosec // unix seconds fit u64
			})
			if len(out) >= max {
				break
			}
		}

		now := rc.Now
		if now.IsZero() {
			now = time.Now()
		}
		ttl := svc.TTL()
		if ttl <= 0 {
			ttl = 10 * time.Minute
		}

		list := &tbproto.BootstrapPeerList{
			IssuerId:  issuer[:],
			SignedAt:  uint64(now.Unix()),          //nolint:gosec
			ExpiresAt: uint64(now.Add(ttl).Unix()), //nolint:gosec
			Peers:     out,
		}
		cb, err := tbproto.CanonicalBootstrapPeerListPreSig(list)
		if err != nil {
			return nil, fmt.Errorf("bootstrap_peers: canonical: %w", err)
		}
		sig, err := svc.Sign(cb)
		if err != nil {
			return &tbproto.RpcResponse{
				Status: tbproto.RpcStatus_RPC_STATUS_INTERNAL,
				Error:  &tbproto.RpcError{Code: "BOOTSTRAP_LIST_SIGN", Message: err.Error()},
			}, nil
		}
		list.Sig = sig
		payload, err := proto.Marshal(list)
		if err != nil {
			return nil, fmt.Errorf("bootstrap_peers: marshal: %w", err)
		}
		return OkResponse(payload), nil
	}
}

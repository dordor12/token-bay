package api

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/stunturn"
)

// turnAllocator is the slice of stunturn.Allocator used by
// turn_relay_open.
type turnAllocator interface {
	Allocate(consumer, seeder idsLike, requestID [16]byte, now time.Time) (stunturn.Session, error)
}

// installTurnRelayOpen wires the live handler when Deps.StunTurn
// satisfies turnAllocator; otherwise the Scope-2 stub.
func (r *Router) installTurnRelayOpen() handlerFunc {
	if r.deps.StunTurn == nil {
		return notImpl("turn_relay_open")
	}
	alloc, ok := r.deps.StunTurn.(turnAllocator)
	if !ok {
		return notImpl("turn_relay_open")
	}
	return func(_ context.Context, rc *RequestCtx, payloadBytes []byte) (*tbproto.RpcResponse, error) {
		var req tbproto.TurnRelayOpenRequest
		if err := proto.Unmarshal(payloadBytes, &req); err != nil {
			return nil, ErrInvalid("TurnRelayOpenRequest: " + err.Error())
		}
		if len(req.SessionId) != 16 {
			return nil, ErrInvalid(fmt.Sprintf("session_id length %d, want 16", len(req.SessionId)))
		}
		var rid [16]byte
		copy(rid[:], req.SessionId)

		// TODO(broker): once internal/broker lands, the seeder identity
		// is recovered from a reservation token rather than reflexively
		// from rc.PeerID. Scope-2 self-loop only.
		sess, err := alloc.Allocate(rc.PeerID, rc.PeerID, rid, rc.Now)
		switch {
		case errors.Is(err, stunturn.ErrThrottled):
			return nil, ErrNoCapacity("turn_relay_open: seeder bandwidth budget exceeded")
		case errors.Is(err, stunturn.ErrDuplicateRequest):
			return nil, ErrInvalid("turn_relay_open: duplicate request_id")
		case err != nil:
			return nil, fmt.Errorf("turn_relay_open: %w", err)
		}

		// relay_endpoint is a placeholder in v1: the real address comes
		// from the server's TURN listener; threading it through Deps is
		// deferred until broker drives the offer. See spec §4.4 / TODO.
		out := &tbproto.TurnRelayOpenResponse{
			RelayEndpoint: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), 0).String(),
			Token:         sess.Token[:],
		}
		b, _ := proto.Marshal(out)
		return OkResponse(b), nil
	}
}

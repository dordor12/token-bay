package api

import (
	"context"
	"errors"
	"fmt"
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

// turnAssignmentLookup recovers the (consumer, seeder) pair the broker bound
// to a reservation_token. *broker.Broker satisfies this structurally.
type turnAssignmentLookup interface {
	LookupAssignment(reqID [16]byte) (consumer, seeder idsLike, ok bool)
}

// fallbackRelayAddr is the v1 placeholder returned when Deps.RelayPublicAddr
// is unset; component tests rely on a non-empty value but make no assertion
// about routability.
const fallbackRelayAddr = "127.0.0.1:0"

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
	// lookup is optional: when nil (no broker wired) the handler falls
	// back to the self-loop semantics tested in Scope-2.
	var lookup turnAssignmentLookup
	if r.deps.Broker != nil {
		lookup = r.deps.Broker
	}
	relayAddr := r.deps.RelayPublicAddr
	if relayAddr == "" {
		relayAddr = fallbackRelayAddr
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

		// Default to self-loop (Scope-2: no broker, both ends are the caller).
		consumer, seeder := rc.PeerID, rc.PeerID
		if lookup != nil {
			c, s, found := lookup.LookupAssignment(rid)
			if !found {
				return nil, ErrInvalid("turn_relay_open: unknown reservation_token")
			}
			// Only the consumer or the assigned seeder may open the relay.
			if rc.PeerID != c && rc.PeerID != s {
				return nil, ErrUnauthenticated("turn_relay_open: caller not party to reservation")
			}
			consumer, seeder = c, s
		}

		sess, err := alloc.Allocate(consumer, seeder, rid, rc.Now)
		switch {
		case errors.Is(err, stunturn.ErrThrottled):
			return nil, ErrNoCapacity("turn_relay_open: seeder bandwidth budget exceeded")
		case errors.Is(err, stunturn.ErrDuplicateRequest):
			return nil, ErrInvalid("turn_relay_open: duplicate request_id")
		case err != nil:
			return nil, fmt.Errorf("turn_relay_open: %w", err)
		}

		out := &tbproto.TurnRelayOpenResponse{
			RelayEndpoint: relayAddr,
			Token:         sess.Token[:],
		}
		b, _ := proto.Marshal(out)
		return OkResponse(b), nil
	}
}

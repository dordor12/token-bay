package api

import (
	"context"
	"net/netip"

	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// stunReflector is the slice of stunturn used by stun_allocate.
type stunReflector interface {
	ReflectAddr(remote netip.AddrPort) netip.AddrPort
}

// installStunAllocate wires the live stun_allocate handler when
// Deps.StunTurn satisfies stunReflector; otherwise the Scope-2 stub.
func (r *Router) installStunAllocate() handlerFunc {
	if r.deps.StunTurn == nil {
		return notImpl("stun_allocate")
	}
	st, ok := r.deps.StunTurn.(stunReflector)
	if !ok {
		return notImpl("stun_allocate")
	}
	return func(_ context.Context, rc *RequestCtx, payloadBytes []byte) (*tbproto.RpcResponse, error) {
		// StunAllocateRequest is currently empty (per shared/proto/rpc.proto)
		// — accept any payload bytes (zero or proto-shaped) without rejection.
		var req tbproto.StunAllocateRequest
		if len(payloadBytes) > 0 {
			if err := proto.Unmarshal(payloadBytes, &req); err != nil {
				return nil, ErrInvalid("StunAllocateRequest: " + err.Error())
			}
		}
		out := &tbproto.StunAllocateResponse{
			ExternalAddr: st.ReflectAddr(rc.RemoteAddr).String(),
		}
		b, err := proto.Marshal(out)
		if err != nil {
			return nil, err
		}
		return OkResponse(b), nil
	}
}

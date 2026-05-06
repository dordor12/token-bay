package api

import (
	"context"

	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// brokerSubmitter is reserved for the broker subsystem. While
// Deps.Broker is nil the handler returns ErrNotImplemented; when
// broker lands the handler swaps to a real implementation in this
// same file (zero changes elsewhere in internal/api).
type brokerSubmitter interface {
	Submit(ctx context.Context, env *tbproto.EnvelopeSigned) (*tbproto.BrokerResponse, error)
}

// installBrokerRequest currently always returns the Scope-2 stub. The
// shape (interface check + closure) is preserved so the future broker
// PR is a one-file edit.
func (r *Router) installBrokerRequest() handlerFunc {
	if r.deps.Broker == nil {
		return notImpl("broker_request")
	}
	br, ok := r.deps.Broker.(brokerSubmitter)
	if !ok {
		return notImpl("broker_request")
	}
	_ = br // not yet exercised — Scope-2 keeps the stub even when wired
	return notImpl("broker_request")
}

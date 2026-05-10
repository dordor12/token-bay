package seederflow

import (
	"context"
	"net/netip"
)

// NopAcceptor is a TunnelAcceptor that never accepts a connection. It
// blocks Accept until ctx is done and returns ctx.Err. Use it in the
// cmd layer for v1 deployments where the tunnel-listener wiring is
// gated on a future shared/proto change (see doc.go "Wire-format
// compatibility note"); HandleOffer + Advertise still function so the
// seeder participates in matchmaking, but no consumer connection
// arrives.
type NopAcceptor struct{}

// Accept blocks until ctx is canceled.
func (NopAcceptor) Accept(ctx context.Context) (TunnelConn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// LocalAddr returns the zero AddrPort.
func (NopAcceptor) LocalAddr() netip.AddrPort { return netip.AddrPort{} }

// Close is a no-op.
func (NopAcceptor) Close() error { return nil }

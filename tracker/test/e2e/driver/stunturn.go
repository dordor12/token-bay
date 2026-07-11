//go:build e2e

package driver

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/pion/stun/v2"
)

// STUNReflexive sends an RFC 5389 binding request to stunAddr and returns the
// XOR-MAPPED-ADDRESS the tracker reflected back.
func STUNReflexive(ctx context.Context, stunAddr string) (netip.AddrPort, error) {
	conn, err := net.Dial("udp", stunAddr)
	if err != nil {
		return netip.AddrPort{}, err
	}
	defer conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)

	req := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
	if _, err := conn.Write(req.Raw); err != nil {
		return netip.AddrPort{}, err
	}
	buf := make([]byte, 1500)
	n, err := conn.Read(buf)
	if err != nil {
		return netip.AddrPort{}, err
	}
	resp := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
	if err := resp.Decode(); err != nil {
		return netip.AddrPort{}, fmt.Errorf("driver: decode STUN response: %w", err)
	}
	var xor stun.XORMappedAddress
	if err := xor.GetFrom(resp); err != nil {
		return netip.AddrPort{}, fmt.Errorf("driver: no XOR-MAPPED-ADDRESS: %w", err)
	}
	addr, ok := netip.AddrFromSlice(xor.IP)
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("driver: bad reflexive ip")
	}
	return netip.AddrPortFrom(addr.Unmap(), uint16(xor.Port)), nil
}

// RelayFrame prefixes the 16-byte session token to payload.
func RelayFrame(token, payload []byte) []byte {
	out := make([]byte, 0, len(token)+len(payload))
	return append(append(out, token...), payload...)
}

// RelayToken returns the 16-byte token prefix of a relay frame.
func RelayToken(frame []byte) []byte {
	if len(frame) < 16 {
		return nil
	}
	return frame[:16]
}

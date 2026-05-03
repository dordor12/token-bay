package stunturn

import "net/netip"

// ReflectResult is the value Reflect returns. The listener writes
// Response to the remote endpoint at Observed.
type ReflectResult struct {
	Observed netip.AddrPort
	Response []byte
}

// Reflect builds a binding-response payload for the observed remote
// address. Returns a zero-value ReflectResult (empty Response, invalid
// Observed) when observed.IsValid() == false; the listener should drop
// the packet in that case.
func Reflect(txID [12]byte, observed netip.AddrPort) ReflectResult {
	if !observed.IsValid() {
		return ReflectResult{}
	}
	return ReflectResult{
		Observed: observed,
		Response: EncodeBindingResponse(txID, observed),
	}
}

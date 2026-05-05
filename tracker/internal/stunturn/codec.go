package stunturn

import (
	"fmt"
	"net"
	"net/netip"

	pionstun "github.com/pion/stun/v2"
)

// expectedRequestAttrs is the set of attribute types the binding-request
// validator accepts in the comprehension-required range. v1 expects an
// empty body — any other comprehension-required attr causes rejection.
//
// Comprehension-required attrs are 0x0000–0x7FFF; comprehension-optional
// attrs (0x8000–0xFFFF) are skipped per RFC 5389 §7.3.1 regardless.
var expectedRequestAttrs = map[pionstun.AttrType]struct{}{}

// DecodeBindingRequest unmarshals p with pion/stun, validates it is a
// binding request with the expected attribute set, and returns the
// 12-byte transaction ID. Returns ErrInvalidPacket (wrapping the
// underlying reason) on any failure.
func DecodeBindingRequest(p []byte) ([12]byte, error) {
	var m pionstun.Message
	if err := m.UnmarshalBinary(p); err != nil {
		return [12]byte{}, fmt.Errorf("%w: %v", ErrInvalidPacket, err)
	}
	if m.Type.Method != pionstun.MethodBinding || m.Type.Class != pionstun.ClassRequest {
		return [12]byte{}, fmt.Errorf("%w: not a binding request", ErrInvalidPacket)
	}
	for _, attr := range m.Attributes {
		if attr.Type >= 0x8000 {
			continue // comprehension-optional, skip
		}
		if _, ok := expectedRequestAttrs[attr.Type]; !ok {
			return [12]byte{}, fmt.Errorf(
				"%w: unknown comprehension-required attribute 0x%04x",
				ErrInvalidPacket, uint16(attr.Type),
			)
		}
	}
	var out [12]byte
	copy(out[:], m.TransactionID[:])
	return out, nil
}

// EncodeBindingResponse builds a STUN binding success response with one
// XOR-MAPPED-ADDRESS attribute and returns the wire bytes.
//
// Result length: 32 bytes for IPv4, 44 bytes for IPv6.
//
// Panics if observed.IsValid() == false. The listener owns input
// validation; passing a zero-value AddrPort is a programmer error.
func EncodeBindingResponse(txID [12]byte, observed netip.AddrPort) []byte {
	if !observed.IsValid() {
		panic("stunturn: EncodeBindingResponse: observed address invalid")
	}

	m := new(pionstun.Message)
	m.TransactionID = txID
	m.Type = pionstun.MessageType{
		Method: pionstun.MethodBinding,
		Class:  pionstun.ClassSuccessResponse,
	}

	addr := observed.Addr()
	var ip net.IP
	if addr.Is4() {
		v4 := addr.As4()
		ip = net.IP(v4[:])
	} else {
		v16 := addr.As16()
		ip = net.IP(v16[:])
	}

	xma := &pionstun.XORMappedAddress{IP: ip, Port: int(observed.Port())}
	if err := xma.AddTo(m); err != nil {
		// pion's AddTo only fails on programmer error (nil receiver,
		// invalid IP family). Both are caller-responsibility above.
		panic(fmt.Sprintf("stunturn: AddTo: %v", err))
	}
	m.Encode()

	out := make([]byte, len(m.Raw))
	copy(out, m.Raw)
	return out
}

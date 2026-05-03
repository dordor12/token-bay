package stunturn

import (
	"errors"
	"net/netip"
	"testing"

	pionstun "github.com/pion/stun/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var refTxID = [12]byte{
	0x01, 0x02, 0x03, 0x04, 0x05, 0x06,
	0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c,
}

// TestEncodeBindingResponse_IPv4_RoundTrip encodes a binding response
// and asserts pion can decode it back to the same address.
func TestEncodeBindingResponse_IPv4_RoundTrip(t *testing.T) {
	observed := netip.MustParseAddrPort("1.2.3.4:50001")

	raw := EncodeBindingResponse(refTxID, observed)
	require.Len(t, raw, 32, "IPv4 binding response is 32 bytes")

	var m pionstun.Message
	require.NoError(t, m.UnmarshalBinary(raw))
	assert.Equal(t, refTxID[:], m.TransactionID[:])

	var xma pionstun.XORMappedAddress
	require.NoError(t, xma.GetFrom(&m))
	assert.True(t, xma.IP.To4() != nil, "address must be IPv4")
	assert.Equal(t, "1.2.3.4", xma.IP.String())
	assert.Equal(t, 50001, xma.Port)
}

// TestEncodeBindingResponse_IPv6_RoundTrip is the same for IPv6.
func TestEncodeBindingResponse_IPv6_RoundTrip(t *testing.T) {
	observed := netip.MustParseAddrPort("[2001:db8::1]:50001")

	raw := EncodeBindingResponse(refTxID, observed)
	require.Len(t, raw, 44, "IPv6 binding response is 44 bytes")

	var m pionstun.Message
	require.NoError(t, m.UnmarshalBinary(raw))
	assert.Equal(t, refTxID[:], m.TransactionID[:])

	var xma pionstun.XORMappedAddress
	require.NoError(t, xma.GetFrom(&m))
	assert.Nil(t, xma.IP.To4(), "address must be IPv6")
	assert.Equal(t, "2001:db8::1", xma.IP.String())
	assert.Equal(t, 50001, xma.Port)
}

// TestEncodeBindingResponse_PanicsOnInvalid asserts the documented
// "caller responsibility" contract.
func TestEncodeBindingResponse_PanicsOnInvalid(t *testing.T) {
	assert.Panics(t, func() {
		_ = EncodeBindingResponse(refTxID, netip.AddrPort{})
	})
}

// TestDecodeBindingRequest_AcceptsPionRequest uses pion to build a
// well-formed request and asserts our wrapper extracts the txID.
func TestDecodeBindingRequest_AcceptsPionRequest(t *testing.T) {
	m, err := pionstun.Build(
		pionstun.NewTransactionIDSetter(refTxID),
		pionstun.BindingRequest,
	)
	require.NoError(t, err)

	got, err := DecodeBindingRequest(m.Raw)

	require.NoError(t, err)
	assert.Equal(t, refTxID, got)
}

func TestDecodeBindingRequest_ShortHeader(t *testing.T) {
	got, err := DecodeBindingRequest(make([]byte, 19))

	assert.Equal(t, [12]byte{}, got)
	assert.True(t, errors.Is(err, ErrInvalidPacket), "want ErrInvalidPacket, got %v", err)
}

func TestDecodeBindingRequest_BindingResponse_Rejected(t *testing.T) {
	// A binding success response, not a request. Our wrapper must reject.
	m, err := pionstun.Build(
		pionstun.NewTransactionIDSetter(refTxID),
		pionstun.BindingSuccess,
	)
	require.NoError(t, err)

	got, decErr := DecodeBindingRequest(m.Raw)

	assert.Equal(t, [12]byte{}, got)
	assert.True(t, errors.Is(decErr, ErrInvalidPacket), "want ErrInvalidPacket, got %v", decErr)
}

func TestDecodeBindingRequest_GarbageBytes(t *testing.T) {
	// Bytes that look like a STUN message length-wise but pion's
	// UnmarshalBinary fails on (e.g., wrong magic cookie).
	junk := make([]byte, 20)
	for i := range junk {
		junk[i] = 0xff
	}

	got, err := DecodeBindingRequest(junk)

	assert.Equal(t, [12]byte{}, got)
	assert.True(t, errors.Is(err, ErrInvalidPacket), "want ErrInvalidPacket, got %v", err)
}

func TestDecodeBindingRequest_UnknownComprehensionRequiredAttr(t *testing.T) {
	// Build a binding request, then add a raw attribute with type 0x0001
	// (USERNAME-class, comprehension-required, but not part of our
	// expected attr set) so our wrapper rejects it.
	m := new(pionstun.Message)
	m.TransactionID = refTxID
	m.Type = pionstun.MessageType{
		Method: pionstun.MethodBinding,
		Class:  pionstun.ClassRequest,
	}
	m.Add(pionstun.AttrType(0x0001), []byte{0xaa, 0xbb, 0xcc, 0xdd})
	m.Encode()

	got, err := DecodeBindingRequest(m.Raw)

	assert.Equal(t, [12]byte{}, got)
	assert.True(t, errors.Is(err, ErrInvalidPacket), "want ErrInvalidPacket, got %v", err)
}

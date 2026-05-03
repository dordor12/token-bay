package stunturn

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReflect_IPv4_DelegatesToEncode(t *testing.T) {
	observed := netip.MustParseAddrPort("1.2.3.4:50001")

	got := Reflect(refTxID, observed)

	assert.Equal(t, observed, got.Observed)
	assert.Equal(t, EncodeBindingResponse(refTxID, observed), got.Response)
}

func TestReflect_IPv6_DelegatesToEncode(t *testing.T) {
	observed := netip.MustParseAddrPort("[2001:db8::1]:50001")

	got := Reflect(refTxID, observed)

	assert.Equal(t, observed, got.Observed)
	assert.Equal(t, EncodeBindingResponse(refTxID, observed), got.Response)
}

func TestReflect_InvalidAddr_EmptyResult(t *testing.T) {
	got := Reflect(refTxID, netip.AddrPort{})

	assert.False(t, got.Observed.IsValid())
	assert.Nil(t, got.Response)
}

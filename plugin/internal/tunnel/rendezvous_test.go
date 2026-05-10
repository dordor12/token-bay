package tunnel

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRelayCoords_Valid(t *testing.T) {
	rc, err := ParseRelayCoords("127.0.0.1:51820", []byte{1, 2, 3, 4})
	require.NoError(t, err)
	assert.Equal(t, netip.MustParseAddrPort("127.0.0.1:51820"), rc.Endpoint)
	assert.Equal(t, []byte{1, 2, 3, 4}, rc.Token)
}

func TestParseRelayCoords_Invalid(t *testing.T) {
	_, err := ParseRelayCoords("not-an-addr", nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig))
}

func TestParseRelayCoords_EmptyEndpoint(t *testing.T) {
	_, err := ParseRelayCoords("", []byte{1})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidConfig))
}

func TestFormatRelayCoords_RoundTrip(t *testing.T) {
	original := RelayCoords{
		Endpoint: netip.MustParseAddrPort("[::1]:51820"),
		Token:    []byte("opaque-token"),
	}
	endpoint, token := FormatRelayCoords(original)
	rc, err := ParseRelayCoords(endpoint, token)
	require.NoError(t, err)
	assert.Equal(t, original, rc)
}

// Compile-time check that the package's noopRendezvous satisfies the public
// Rendezvous interface. Failure means the interface signature regressed.
var _ Rendezvous = (*noopRendezvous)(nil)

type noopRendezvous struct{}

func (noopRendezvous) AllocateReflexive(_ context.Context) (netip.AddrPort, error) {
	return netip.AddrPort{}, nil
}

func (noopRendezvous) OpenRelay(_ context.Context, _ [16]byte) (RelayCoords, error) {
	return RelayCoords{}, nil
}

package server

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/tracker/internal/stunturn"
)

func ap(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

func TestRelayRouter_LearnsTwoSidesAndForwards(t *testing.T) {
	r := newRelayRouter()
	tok := stunturn.Token{0x01}
	t0 := time.Unix(1700000000, 0)
	a, b := ap("10.0.0.1:5000"), ap("10.0.0.2:6000")

	// First datagram: side A bound, no peer yet.
	dst, ok := r.peerFor(tok, a, t0)
	require.False(t, ok, "no peer bound yet")
	require.Equal(t, netip.AddrPort{}, dst)

	// Second distinct src: side B bound, peer is A.
	dst, ok = r.peerFor(tok, b, t0)
	require.True(t, ok)
	require.Equal(t, a, dst)

	// A→ forwards to B; B→ forwards to A.
	dst, ok = r.peerFor(tok, a, t0)
	require.True(t, ok)
	require.Equal(t, b, dst)
	dst, ok = r.peerFor(tok, b, t0)
	require.True(t, ok)
	require.Equal(t, a, dst)

	// A third distinct src is dropped.
	_, ok = r.peerFor(tok, ap("10.0.0.3:7000"), t0)
	require.False(t, ok)
	assert.Equal(t, 1, r.activeCount())
}

func TestRelayRouter_ReapRemovesIdle(t *testing.T) {
	r := newRelayRouter()
	tok := stunturn.Token{0x02}
	t0 := time.Unix(1700000000, 0)
	r.peerFor(tok, ap("10.0.0.1:5000"), t0)
	assert.Equal(t, 1, r.activeCount())
	assert.Equal(t, 1, r.reap(t0.Add(time.Minute)))
	assert.Equal(t, 0, r.activeCount())
}

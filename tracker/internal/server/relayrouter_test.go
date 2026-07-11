package server

import (
	"fmt"
	"net/netip"
	"sync"
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

// TestRelayRouter_ConcurrentPeerForAndReap exercises the router under its
// real Task-3 concurrent usage: the TURN read loop calling peerFor from many
// distinct (token, src) pairs while the reaper concurrently sweeps idle
// bindings. peerFor here is only ever reached after ResolveAndCharge has
// validated the token in the real data plane, so all traffic modeled below
// is "authorized" — the router's lastSeen-refresh-on-any-source behavior is
// intended, not a bug under test.
func TestRelayRouter_ConcurrentPeerForAndReap(t *testing.T) {
	r := newRelayRouter()
	const goroutines = 8
	const iterations = 200

	var writers sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		writers.Add(1)
		go func(g int) {
			defer writers.Done()
			tok := stunturn.Token{byte(g + 1)}
			src := ap(fmt.Sprintf("10.1.%d.1:%d", g, 5000+g))
			for i := 0; i < iterations; i++ {
				_, _ = r.peerFor(tok, src, time.Now())
			}
		}(g)
	}

	// A concurrent reaper races the writers. A far-past cutoff never removes
	// a binding that's actively being refreshed, so this only exercises the
	// lock/contention path, not eviction.
	stopReap := make(chan struct{})
	var reaper sync.WaitGroup
	reaper.Add(1)
	go func() {
		defer reaper.Done()
		for {
			select {
			case <-stopReap:
				return
			default:
				r.reap(time.Now().Add(-time.Hour))
			}
		}
	}()

	writers.Wait()
	close(stopReap)
	reaper.Wait()

	assert.Equal(t, goroutines, r.activeCount(), "each goroutine bound a distinct token; none should have been reaped")

	// A final reap with a future cutoff removes everything — a sane end state.
	assert.Equal(t, goroutines, r.reap(time.Now().Add(time.Hour)))
	assert.Equal(t, 0, r.activeCount())
}

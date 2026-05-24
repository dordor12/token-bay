package federation

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
)

func TestGossipLimiter_DisabledBucketAllowsAll(t *testing.T) {
	now := time.Unix(10000, 0)
	g := newGossipLimiter(RateLimitConfig{
		Revocation: RateBucket{Rate: 0}, // disabled
	}, func() time.Time { return now })
	var p ids.TrackerID
	p[0] = 0xAA
	for i := 0; i < 1000; i++ {
		require.True(t, g.Allow(p, fed.Kind_KIND_REVOCATION), "rate=0 must allow all")
	}
}

func TestGossipLimiter_BurstThenThrottle(t *testing.T) {
	now := time.Unix(10000, 0)
	clock := now
	g := newGossipLimiter(RateLimitConfig{
		Revocation: RateBucket{Rate: 1, Burst: 3},
	}, func() time.Time { return clock })
	var p ids.TrackerID
	p[0] = 0xBB
	// First 3 calls drain the burst.
	for i := 0; i < 3; i++ {
		require.True(t, g.Allow(p, fed.Kind_KIND_REVOCATION), "burst sample %d", i)
	}
	// 4th call at the same instant — rejected.
	require.False(t, g.Allow(p, fed.Kind_KIND_REVOCATION))
	// Advance 1s — 1 token refilled.
	clock = now.Add(time.Second)
	require.True(t, g.Allow(p, fed.Kind_KIND_REVOCATION))
	require.False(t, g.Allow(p, fed.Kind_KIND_REVOCATION))
}

func TestGossipLimiter_PerPeerIsolation(t *testing.T) {
	now := time.Unix(10000, 0)
	g := newGossipLimiter(RateLimitConfig{
		Revocation: RateBucket{Rate: 0.1, Burst: 1},
	}, func() time.Time { return now })
	var p1, p2 ids.TrackerID
	p1[0] = 0x01
	p2[0] = 0x02
	require.True(t, g.Allow(p1, fed.Kind_KIND_REVOCATION))
	require.False(t, g.Allow(p1, fed.Kind_KIND_REVOCATION))
	// p2 has its own bucket; first call still allowed.
	require.True(t, g.Allow(p2, fed.Kind_KIND_REVOCATION))
	require.False(t, g.Allow(p2, fed.Kind_KIND_REVOCATION))
}

func TestGossipLimiter_PerKindIsolation(t *testing.T) {
	now := time.Unix(10000, 0)
	g := newGossipLimiter(RateLimitConfig{
		Revocation:      RateBucket{Rate: 0.1, Burst: 1},
		PeerExchange:    RateBucket{Rate: 0.1, Burst: 1},
		RootAttestation: RateBucket{Rate: 0.1, Burst: 1},
	}, func() time.Time { return now })
	var p ids.TrackerID
	p[0] = 0x03
	// Each kind has its own bucket; one call per kind succeeds even when
	// another kind's bucket is empty.
	require.True(t, g.Allow(p, fed.Kind_KIND_REVOCATION))
	require.False(t, g.Allow(p, fed.Kind_KIND_REVOCATION))
	require.True(t, g.Allow(p, fed.Kind_KIND_PEER_EXCHANGE))
	require.True(t, g.Allow(p, fed.Kind_KIND_ROOT_ATTESTATION))
}

func TestGossipLimiter_Concurrent(t *testing.T) {
	now := time.Unix(10000, 0)
	g := newGossipLimiter(RateLimitConfig{
		Revocation: RateBucket{Rate: 100, Burst: 50},
	}, func() time.Time { return now })
	var p ids.TrackerID
	p[0] = 0x04
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = g.Allow(p, fed.Kind_KIND_REVOCATION)
			}
		}()
	}
	wg.Wait()
}

package trackerclient

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/token-bay/token-bay/shared/ids"
)

func newBP(addr string, score float64, idByte byte) BootstrapPeer {
	var tid ids.IdentityID
	copy(tid[:], bytes.Repeat([]byte{idByte}, 32))
	return BootstrapPeer{
		TrackerID:   tid,
		Addr:        addr,
		RegionHint:  "r",
		HealthScore: score,
		LastSeen:    time.Unix(1, 0),
	}
}

func TestPeerCandidates_EmptyReturnsBase(t *testing.T) {
	now := time.Unix(10000, 0)
	pc := newPeerCandidates(func() time.Time { return now })
	base := []TrackerEndpoint{{Addr: "a:1", IdentityHash: ids.IdentityID{0x01}}}
	ep, src := pc.NextEndpoint(0, base)
	require.Equal(t, "a:1", ep.Addr)
	require.Equal(t, "base", src)
}

func TestPeerCandidates_MergesAndRanksByHealth(t *testing.T) {
	now := time.Unix(10000, 0)
	pc := newPeerCandidates(func() time.Time { return now })
	base := []TrackerEndpoint{{Addr: "a:1", IdentityHash: ids.IdentityID{0x01}}}

	pc.Update([]BootstrapPeer{
		newBP("c:3", 0.4, 0x03),
		newBP("b:2", 0.9, 0x02),
	}, now.Add(10*time.Minute))

	// idx=0 → base
	ep, src := pc.NextEndpoint(0, base)
	require.Equal(t, "a:1", ep.Addr)
	require.Equal(t, "base", src)

	// idx=1 → first candidate by health (b:2, score 0.9)
	ep, src = pc.NextEndpoint(1, base)
	require.Equal(t, "b:2", ep.Addr)
	require.Equal(t, "candidate", src)

	// idx=2 → second candidate (c:3, score 0.4)
	ep, src = pc.NextEndpoint(2, base)
	require.Equal(t, "c:3", ep.Addr)
	require.Equal(t, "candidate", src)

	// idx=3 wraps → base
	ep, src = pc.NextEndpoint(3, base)
	require.Equal(t, "a:1", ep.Addr)
	require.Equal(t, "base", src)
}

func TestPeerCandidates_ExpiredFallsBackToBase(t *testing.T) {
	now := time.Unix(10000, 0)
	clock := now
	pc := newPeerCandidates(func() time.Time { return clock })
	base := []TrackerEndpoint{{Addr: "a:1", IdentityHash: ids.IdentityID{0x01}}}

	pc.Update([]BootstrapPeer{newBP("b:2", 0.9, 0x02)}, now.Add(5*time.Second))

	// before expiry → uses candidate
	ep, src := pc.NextEndpoint(1, base)
	require.Equal(t, "b:2", ep.Addr)
	require.Equal(t, "candidate", src)

	// advance past expiry → only base
	clock = now.Add(10 * time.Second)
	ep, src = pc.NextEndpoint(1, base)
	require.Equal(t, "a:1", ep.Addr, "candidate dropped after expiry")
	require.Equal(t, "base", src)
}

func TestPeerCandidates_DropsAddrCollisionWithBase(t *testing.T) {
	now := time.Unix(10000, 0)
	pc := newPeerCandidates(func() time.Time { return now })
	base := []TrackerEndpoint{{Addr: "a:1", IdentityHash: ids.IdentityID{0x01}}}

	// Candidate has same Addr as base — must be dropped.
	pc.Update([]BootstrapPeer{
		newBP("a:1", 1.0, 0xFF), // mismatched IdentityHash, same Addr
		newBP("c:3", 0.5, 0x03),
	}, now.Add(10*time.Minute))

	ep, src := pc.NextEndpoint(1, base)
	require.Equal(t, "c:3", ep.Addr, "duplicate-Addr candidate dropped")
	require.Equal(t, "candidate", src)
}

func TestPeerCandidates_TieBreakByAddrAscending(t *testing.T) {
	now := time.Unix(10000, 0)
	pc := newPeerCandidates(func() time.Time { return now })
	base := []TrackerEndpoint{{Addr: "a:1", IdentityHash: ids.IdentityID{0x01}}}

	pc.Update([]BootstrapPeer{
		newBP("z:9", 0.5, 0x05),
		newBP("b:2", 0.5, 0x02), // same score as z:9
		newBP("m:5", 0.5, 0x06),
	}, now.Add(10*time.Minute))

	ep, _ := pc.NextEndpoint(1, base)
	require.Equal(t, "b:2", ep.Addr, "lexicographic tie-break")
	ep, _ = pc.NextEndpoint(2, base)
	require.Equal(t, "m:5", ep.Addr)
	ep, _ = pc.NextEndpoint(3, base)
	require.Equal(t, "z:9", ep.Addr)
}

func TestPeerCandidates_Concurrent(t *testing.T) {
	now := time.Unix(10000, 0)
	pc := newPeerCandidates(func() time.Time { return now })
	base := []TrackerEndpoint{{Addr: "a:1", IdentityHash: ids.IdentityID{0x01}}}

	done := make(chan struct{}, 2)
	go func() {
		for i := 0; i < 200; i++ {
			pc.Update([]BootstrapPeer{newBP("b:2", float64(i)/200.0, 0x02)}, now.Add(time.Minute))
		}
		done <- struct{}{}
	}()
	go func() {
		for i := 0; i < 200; i++ {
			_, _ = pc.NextEndpoint(i, base)
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}

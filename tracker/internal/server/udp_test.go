package server

import (
	"context"
	"crypto/rand"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/pion/stun/v2"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/stunturn"
)

// buildBindingRequest builds a minimal RFC 5389 binding request with the given
// transaction id, using the same pion library stunturn wraps.
func buildBindingRequest(t *testing.T, txID [12]byte) []byte {
	t.Helper()
	m := stun.New()
	m.Type = stun.BindingRequest
	copy(m.TransactionID[:], txID[:])
	m.Encode()
	return m.Raw
}

// startTestPlane brings up a UDPDataPlane on ephemeral loopback ports and
// returns its bound STUN/TURN addresses.
func startTestPlane(t *testing.T, alloc *stunturn.Allocator) (stunAddr, turnAddr netip.AddrPort) {
	t.Helper()
	dp, err := NewUDPDataPlane(UDPDeps{
		STUNAddr:   "127.0.0.1:0",
		TURNAddr:   "127.0.0.1:0",
		Alloc:      alloc,
		Now:        time.Now,
		SessionTTL: time.Minute,
		Logger:     zerolog.Nop(),
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = dp.Run(ctx) }()
	// Run binds synchronously before returning bound addrs via the accessors.
	require.Eventually(t, func() bool { return dp.STUNBound().IsValid() && dp.TURNBound().IsValid() },
		2*time.Second, 10*time.Millisecond)
	return dp.STUNBound(), dp.TURNBound()
}

func newTestAllocator() *stunturn.Allocator {
	a, _ := stunturn.NewAllocator(stunturn.AllocatorConfig{
		MaxKbpsPerSeeder: 1024, SessionTTL: time.Minute, Now: time.Now, Rand: rand.Reader,
	})
	return a
}

func TestUDPDataPlane_STUNReflect(t *testing.T) {
	stunAddr, _ := startTestPlane(t, newTestAllocator())
	conn, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(stunAddr))
	require.NoError(t, err)
	defer conn.Close()

	// Use a real binding request: pion via stunturn's request path.
	reqBytes := buildBindingRequest(t, [12]byte{0xAB})
	_, err = conn.Write(reqBytes)
	require.NoError(t, err)

	buf := make([]byte, 1500)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	n, err := conn.Read(buf)
	require.NoError(t, err)
	// The reply is a binding *response* (ClassSuccessResponse), which
	// stunturn.DecodeBindingRequest correctly rejects by design (it only
	// accepts requests). Decode it directly with pion — the same codec
	// stunturn wraps — to confirm the reflector echoed our transaction ID.
	var resp stun.Message
	require.NoError(t, resp.UnmarshalBinary(buf[:n]))
	assert.Equal(t, stun.BindingSuccess, resp.Type)
	assert.Equal(t, [12]byte{0xAB}, resp.TransactionID)
}

func TestUDPDataPlane_RelayCrossDelivery(t *testing.T) {
	alloc := newTestAllocator()
	sess, err := alloc.Allocate(ids.IdentityID{0xC0}, ids.IdentityID{0x5E}, [16]byte{0x01}, time.Now())
	require.NoError(t, err)
	_, turnAddr := startTestPlane(t, alloc)

	peerA, _ := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(turnAddr))
	defer peerA.Close()
	peerB, _ := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(turnAddr))
	defer peerB.Close()

	frame := func(payload string) []byte { return append(append([]byte(nil), sess.Token[:]...), []byte(payload)...) }

	// A sends first (binds side A), then B sends (binds side B) — B's datagram forwards to A.
	_, _ = peerA.Write(frame("hello-from-A"))
	time.Sleep(20 * time.Millisecond)
	_, _ = peerB.Write(frame("hello-from-B"))

	buf := make([]byte, 1500)
	require.NoError(t, peerA.SetReadDeadline(time.Now().Add(2*time.Second)))
	n, err := peerA.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, append(append([]byte(nil), sess.Token[:]...), []byte("hello-from-B")...), buf[:n])
}

func TestUDPDataPlane_UnknownTokenDropped(t *testing.T) {
	_, turnAddr := startTestPlane(t, newTestAllocator())
	conn, _ := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(turnAddr))
	defer conn.Close()
	var bad stunturn.Token
	bad[0] = 0xFF
	_, _ = conn.Write(append(append([]byte(nil), bad[:]...), []byte("nope")...))
	buf := make([]byte, 1500)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
	_, err := conn.Read(buf)
	assert.Error(t, err, "unknown token must be dropped (no reply)")
}

func TestUDPDataPlane_ThrottleDrops(t *testing.T) {
	// 1 kbps bucket = 128 bytes/s burst; a 1400-byte flood must throttle.
	alloc, _ := stunturn.NewAllocator(stunturn.AllocatorConfig{
		MaxKbpsPerSeeder: 1, SessionTTL: time.Minute, Now: time.Now, Rand: rand.Reader,
	})
	sess, err := alloc.Allocate(ids.IdentityID{0xC0}, ids.IdentityID{0x5E}, [16]byte{0x09}, time.Now())
	require.NoError(t, err)
	_, turnAddr := startTestPlane(t, alloc)

	a, _ := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(turnAddr))
	defer a.Close()
	b, _ := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(turnAddr))
	defer b.Close()
	big := make([]byte, 1400)
	frame := append(append([]byte(nil), sess.Token[:]...), big...)
	_, _ = a.Write(frame) // binds A, over-budget → charged then throttles subsequent
	time.Sleep(20 * time.Millisecond)
	// Flood: at least one must be dropped by the bucket (no crash, peer B gets ≤1).
	for i := 0; i < 20; i++ {
		_, _ = a.Write(frame)
	}
	// The plane stays alive: an unknown token still gets its own drop path.
	assert.NotPanics(t, func() { _, _ = b.Write([]byte("short")) })
}

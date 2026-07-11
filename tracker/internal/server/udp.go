package server

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/tracker/internal/stunturn"
)

// UDPDeps are the collaborators the UDP data plane needs.
type UDPDeps struct {
	STUNAddr   string // e.g. ":3478"
	TURNAddr   string // e.g. ":3479"
	Alloc      *stunturn.Allocator
	Now        func() time.Time
	SessionTTL time.Duration
	Logger     zerolog.Logger
}

// UDPDataPlane runs the tracker's STUN reflector (:3478) and TURN relay (:3479)
// UDP loops. It owns the sockets and the per-token peer bindings; session auth
// and rate limiting are delegated to the pure stunturn.Allocator.
type UDPDataPlane struct {
	deps    UDPDeps
	router  *relayRouter
	metrics *udpMetrics

	stunBound atomic.Pointer[netip.AddrPort]
	turnBound atomic.Pointer[netip.AddrPort]
}

func NewUDPDataPlane(deps UDPDeps) (*UDPDataPlane, error) {
	if deps.Alloc == nil || deps.Now == nil || deps.SessionTTL <= 0 {
		return nil, errors.New("server: UDPDataPlane requires Alloc, Now, SessionTTL")
	}
	dp := &UDPDataPlane{deps: deps, router: newRelayRouter()}
	dp.metrics = newUDPMetrics(func() float64 { return float64(dp.router.activeCount()) })
	return dp, nil
}

func (dp *UDPDataPlane) Collectors() []prometheus.Collector { return dp.metrics.collectors() }

func (dp *UDPDataPlane) STUNBound() netip.AddrPort {
	if p := dp.stunBound.Load(); p != nil {
		return *p
	}
	return netip.AddrPort{}
}

func (dp *UDPDataPlane) TURNBound() netip.AddrPort {
	if p := dp.turnBound.Load(); p != nil {
		return *p
	}
	return netip.AddrPort{}
}

// Run opens both sockets and serves until ctx is cancelled, then closes them.
func (dp *UDPDataPlane) Run(ctx context.Context) error {
	stunConn, err := listenUDP(dp.deps.STUNAddr)
	if err != nil {
		return err
	}
	turnConn, err := listenUDP(dp.deps.TURNAddr)
	if err != nil {
		_ = stunConn.Close()
		return err
	}
	sb := stunConn.LocalAddr().(*net.UDPAddr).AddrPort()
	tb := turnConn.LocalAddr().(*net.UDPAddr).AddrPort()
	dp.stunBound.Store(&sb)
	dp.turnBound.Store(&tb)

	go func() { <-ctx.Done(); _ = stunConn.Close(); _ = turnConn.Close() }()

	done := make(chan struct{}, 3)
	go func() { dp.stunLoop(stunConn); done <- struct{}{} }()
	go func() { dp.turnLoop(turnConn); done <- struct{}{} }()
	go func() { dp.reapLoop(ctx); done <- struct{}{} }()
	<-ctx.Done()
	for i := 0; i < 3; i++ {
		<-done
	}
	return nil
}

func listenUDP(addr string) (*net.UDPConn, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	return net.ListenUDP("udp", ua)
}

func (dp *UDPDataPlane) stunLoop(conn *net.UDPConn) {
	buf := make([]byte, 1500)
	for {
		n, src, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return // socket closed on shutdown
		}
		txID, derr := stunturn.DecodeBindingRequest(buf[:n])
		if derr != nil {
			dp.metrics.stunRequests.WithLabelValues("invalid").Inc()
			continue
		}
		resp := stunturn.Reflect(txID, src).Response
		_, _ = conn.WriteToUDPAddrPort(resp, src)
		dp.metrics.stunRequests.WithLabelValues("reflected").Inc()
	}
}

func (dp *UDPDataPlane) turnLoop(conn *net.UDPConn) {
	buf := make([]byte, 1500)
	for {
		n, src, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		if n < len(stunturn.Token{}) {
			dp.metrics.turnDatagrams.WithLabelValues("malformed").Inc()
			continue
		}
		var tok stunturn.Token
		copy(tok[:], buf[:len(tok)])
		now := dp.deps.Now()
		if _, cerr := dp.deps.Alloc.ResolveAndCharge(tok, n, now); cerr != nil {
			switch {
			case errors.Is(cerr, stunturn.ErrThrottled):
				dp.metrics.turnDatagrams.WithLabelValues("throttled").Inc()
			default: // ErrUnknownToken / ErrSessionExpired
				dp.metrics.turnDatagrams.WithLabelValues("unknown_token").Inc()
			}
			continue
		}
		dst, ok := dp.router.peerFor(tok, src, now)
		if !ok {
			dp.metrics.turnDatagrams.WithLabelValues("awaiting_peer").Inc()
			continue
		}
		// Forward token+payload verbatim so the receiving peer can demux.
		out := make([]byte, n)
		copy(out, buf[:n])
		_, _ = conn.WriteToUDPAddrPort(out, dst)
		dp.metrics.turnDatagrams.WithLabelValues("relayed").Inc()
		dp.metrics.turnBytes.Add(float64(n))
	}
}

func (dp *UDPDataPlane) reapLoop(ctx context.Context) {
	tick := time.NewTicker(dp.deps.SessionTTL)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			dp.deps.Alloc.Sweep(now)
			dp.router.reap(now.Add(-dp.deps.SessionTTL))
		}
	}
}

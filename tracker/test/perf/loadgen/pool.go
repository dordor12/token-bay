//go:build perf

package loadgen

import (
	"fmt"
	"net"
	"sync/atomic"

	quicgo "github.com/quic-go/quic-go"
)

// TransportPool is a fixed set of UDP sockets wrapped in
// quic.Transports. Thousands of simulated participants dial through a
// bounded socket pool round-robin instead of opening one socket each —
// 20k QUIC connections otherwise means 20k file descriptors.
type TransportPool struct {
	trs  []*quicgo.Transport
	next atomic.Uint64
}

// NewTransportPool binds n UDP sockets on ephemeral ports.
func NewTransportPool(n int) (*TransportPool, error) {
	p := &TransportPool{}
	for i := 0; i < n; i++ {
		conn, err := net.ListenUDP("udp", &net.UDPAddr{})
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("loadgen: bind udp socket %d/%d: %w", i+1, n, err)
		}
		p.trs = append(p.trs, &quicgo.Transport{Conn: conn})
	}
	return p, nil
}

// Next returns the next transport round-robin.
func (p *TransportPool) Next() *quicgo.Transport {
	return p.trs[p.next.Add(1)%uint64(len(p.trs))]
}

// Close closes every transport (and its UDP socket).
func (p *TransportPool) Close() {
	for _, tr := range p.trs {
		_ = tr.Close()
	}
	p.trs = nil
}

package server

import (
	"crypto/tls"
	"net"
	"time"

	quicgo "github.com/quic-go/quic-go"

	"github.com/token-bay/token-bay/tracker/internal/config"
)

// StartListener binds a QUIC listener on cfg.ListenAddr with our mTLS
// contract and quic-go config knobs. The returned *quicgo.Listener
// owns the underlying *net.UDPConn and closes it on Close.
//
// MaxIdleTimeout / MaxIncomingStreams come from cfg; ALPN
// "tokenbay/1" is locked by tls.go. EnableDatagrams + Allow0RTT are
// off — they conflict with the plugin trackerclient's contract.
func StartListener(cfg *config.ServerConfig, cert tls.Certificate) (*quicgo.Listener, error) {
	tlsCfg := MakeServerTLSConfig(cert)
	qcfg := &quicgo.Config{
		EnableDatagrams:    false,
		Allow0RTT:          false,
		MaxIdleTimeout:     time.Duration(cfg.IdleTimeoutS) * time.Second,
		MaxIncomingStreams: int64(cfg.MaxIncomingStreams),
	}
	udpAddr, err := net.ResolveUDPAddr("udp", cfg.ListenAddr)
	if err != nil {
		return nil, err
	}
	pc, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}
	l, err := quicgo.Listen(pc, tlsCfg, qcfg)
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	return l, nil
}

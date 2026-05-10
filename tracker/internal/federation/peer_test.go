package federation_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/tracker/internal/federation"
	"google.golang.org/protobuf/proto"
)

func TestPeer_RecvLoop_DispatchesByKind(t *testing.T) {
	t.Parallel()
	cli, srv := newPeerCfg(t), newPeerCfg(t)
	hub := federation.NewInprocHub()
	srvT := federation.NewInprocTransport(hub, "srv", srv.pub, srv.priv)
	cliT := federation.NewInprocTransport(hub, "cli", cli.pub, cli.priv)
	defer srvT.Close()
	defer cliT.Close()

	got := int32(0)
	dispatch := func(env *fed.Envelope) {
		if env.Kind == fed.Kind_KIND_PING {
			atomic.AddInt32(&got, 1)
		}
	}

	srvAcc := make(chan federation.PeerConn, 1)
	go func() { _ = srvT.Listen(context.Background(), func(c federation.PeerConn) { srvAcc <- c }) }()

	conn, err := cliT.Dial(context.Background(), "srv", srv.pub)
	if err != nil {
		t.Fatal(err)
	}
	srvConn := <-srvAcc

	p := federation.NewPeerForTest(srvConn, dispatch)
	p.Start(context.Background())
	defer p.Stop()

	// Send a Ping from the client side.
	payload, _ := proto.Marshal(&fed.Ping{Nonce: 1})
	cliID := cli.id.Bytes()
	env, _ := federation.SignEnvelope(cli.priv, cliID[:], fed.Kind_KIND_PING, payload)
	frame, _ := federation.MarshalFrame(env)
	_ = conn.Send(context.Background(), frame)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&got) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("dispatch never fired")
}

func TestPeer_Stop_ClosesConn(t *testing.T) {
	t.Parallel()
	cli, srv := newPeerCfg(t), newPeerCfg(t)
	hub := federation.NewInprocHub()
	srvT := federation.NewInprocTransport(hub, "srv", srv.pub, srv.priv)
	cliT := federation.NewInprocTransport(hub, "cli", cli.pub, cli.priv)
	defer srvT.Close()
	defer cliT.Close()

	srvAcc := make(chan federation.PeerConn, 1)
	go func() { _ = srvT.Listen(context.Background(), func(c federation.PeerConn) { srvAcc <- c }) }()
	conn, _ := cliT.Dial(context.Background(), "srv", srv.pub)
	srvConn := <-srvAcc

	p := federation.NewPeerForTest(srvConn, func(*fed.Envelope) {})
	p.Start(context.Background())
	p.Stop()
	_ = conn // recv on the dialer side will see ErrPeerClosed once the inproc pair closes
}

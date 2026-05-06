package server

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/api"
	"github.com/token-bay/token-bay/tracker/internal/config"
)

// fakeDispatcher records calls + returns a fixed response.
type fakeDispatcher struct {
	mu        sync.Mutex
	called    int
	resp      *tbproto.RpcResponse
	doPanic   bool
	gotMethod tbproto.RpcMethod
}

func (f *fakeDispatcher) Dispatch(_ context.Context, _ *api.RequestCtx, req *tbproto.RpcRequest) *tbproto.RpcResponse {
	f.mu.Lock()
	f.called++
	f.gotMethod = req.Method
	doPanic := f.doPanic
	resp := f.resp
	f.mu.Unlock()
	if doPanic {
		panic("test: handler panic")
	}
	return resp
}
func (f *fakeDispatcher) PushAPI() api.PushAPI { return api.PushAPI{} }

// pipeStream wraps a net.Conn so it satisfies the rpcStream interface
// (Read, Write, Close — all already on net.Conn).
type pipeStream struct{ net.Conn }

func (p pipeStream) CloseWrite() error { return p.Close() }

func newTestServer(t *testing.T, fd *fakeDispatcher) *Server {
	t.Helper()
	s, err := New(Deps{
		Config: &config.Config{Server: config.ServerConfig{
			MaxFrameSize:       1 << 20,
			IdleTimeoutS:       60,
			MaxIncomingStreams: 1024,
			ShutdownGraceS:     5,
		}},
		Logger: zerolog.Nop(),
		Now:    time.Now,
		API:    fd,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mkConn(peer ids.IdentityID) *Connection {
	c := &Connection{
		peerID:     peer,
		remoteAddr: netip.MustParseAddrPort("127.0.0.1:55001"),
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	return c
}

func TestHandleRPCStream_HappyPath(t *testing.T) {
	srvSide, cliSide := net.Pipe()
	defer srvSide.Close()
	defer cliSide.Close()

	fd := &fakeDispatcher{
		resp: api.OkResponse([]byte("hello")),
	}
	s := newTestServer(t, fd)
	c := mkConn(ids.IdentityID{0x55})

	// Goroutine: handle one request from the server side.
	done := make(chan struct{})
	go func() {
		s.handleRPCStream(context.Background(), pipeStream{srvSide}, c)
		close(done)
	}()

	// Client side: write a Balance request, read the response.
	if err := WriteFrame(cliSide, &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BALANCE,
		Payload: []byte{1, 2, 3},
	}, 1<<20); err != nil {
		t.Fatal(err)
	}
	var resp tbproto.RpcResponse
	if err := ReadFrame(cliSide, &resp, 1<<20); err != nil {
		t.Fatal(err)
	}
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status = %v", resp.Status)
	}
	if !bytes.Equal(resp.Payload, []byte("hello")) {
		t.Fatalf("payload = %q", resp.Payload)
	}

	<-done
	if fd.called != 1 {
		t.Fatalf("dispatcher called %d times", fd.called)
	}
	if fd.gotMethod != tbproto.RpcMethod_RPC_METHOD_BALANCE {
		t.Fatalf("got method %v", fd.gotMethod)
	}
}

func TestHandleRPCStream_HandlerPanics_InternalPANIC(t *testing.T) {
	srvSide, cliSide := net.Pipe()
	defer srvSide.Close()
	defer cliSide.Close()

	fd := &fakeDispatcher{doPanic: true}
	s := newTestServer(t, fd)
	c := mkConn(ids.IdentityID{0x55})

	done := make(chan struct{})
	go func() {
		s.handleRPCStream(context.Background(), pipeStream{srvSide}, c)
		close(done)
	}()

	if err := WriteFrame(cliSide, &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_BALANCE,
	}, 1<<20); err != nil {
		t.Fatal(err)
	}
	var resp tbproto.RpcResponse
	if err := ReadFrame(cliSide, &resp, 1<<20); err != nil {
		t.Fatal(err)
	}
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INTERNAL ||
		resp.Error == nil || resp.Error.Code != "PANIC" {
		t.Fatalf("got %+v / %+v", resp.Status, resp.Error)
	}
	<-done
}

func TestHandleRPCStream_OversizeFrame_Closes(t *testing.T) {
	srvSide, cliSide := net.Pipe()
	defer srvSide.Close()
	defer cliSide.Close()

	fd := &fakeDispatcher{}
	s := newTestServer(t, fd)
	// Cap reads at 1 KiB so a header claiming 4 KiB triggers oversize,
	// but leaves enough headroom for the server's error response write.
	s.deps.Config.Server.MaxFrameSize = 1024
	c := mkConn(ids.IdentityID{})

	done := make(chan struct{})
	go func() {
		s.handleRPCStream(context.Background(), pipeStream{srvSide}, c)
		close(done)
	}()

	// Write a header claiming 4 KiB (4 × maxFrame).
	if _, err := cliSide.Write([]byte{0x00, 0x00, 0x10, 0x00}); err != nil {
		t.Fatal(err)
	}
	// Server should reply with INVALID/FRAME and close.
	var resp tbproto.RpcResponse
	if err := ReadFrame(cliSide, &resp, 1<<20); err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != "FRAME" {
		t.Fatalf("got %+v / %+v", resp.Status, resp.Error)
	}
	<-done
	if fd.called != 0 {
		t.Fatalf("dispatcher called for oversize request")
	}
}

func TestHandleRPCStream_TruncatedHeader_NoResponse(t *testing.T) {
	srvSide, cliSide := net.Pipe()
	defer srvSide.Close()

	fd := &fakeDispatcher{}
	s := newTestServer(t, fd)
	c := mkConn(ids.IdentityID{})

	done := make(chan struct{})
	go func() {
		s.handleRPCStream(context.Background(), pipeStream{srvSide}, c)
		close(done)
	}()

	// Write 2 bytes then close the client side.
	_, _ = cliSide.Write([]byte{0x00, 0x01})
	_ = cliSide.Close()

	<-done
	if fd.called != 0 {
		t.Fatalf("dispatcher called for truncated header")
	}
}

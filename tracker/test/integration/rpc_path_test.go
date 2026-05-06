//go:build integration

package integration_test

import (
	"context"
	"sync"
	"testing"

	quicgo "github.com/quic-go/quic-go"
	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/server"
)

// rpcSimple opens a stream on conn, writes the request, reads the response.
func rpcSimple(t *testing.T, conn *quicgo.Conn, method tbproto.RpcMethod, payload []byte) *tbproto.RpcResponse {
	t.Helper()
	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err := server.WriteFrame(stream, &tbproto.RpcRequest{Method: method, Payload: payload}, 1<<20); err != nil {
		t.Fatal(err)
	}
	var resp tbproto.RpcResponse
	if err := server.ReadFrame(stream, &resp, 1<<20); err != nil {
		t.Fatalf("read response (method=%v): %v", method, err)
	}
	return &resp
}

// openHB opens the heartbeat stream on conn so the server's
// serveConn proceeds past its first AcceptStream into the RPC loop.
func openHB(t *testing.T, conn *quicgo.Conn) {
	t.Helper()
	hb, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hb.Close() })
}

func TestIntegration_RPC_Balance_OK(t *testing.T) {
	f := newFixture(t)
	conn, err := f.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "bye")
	openHB(t, conn)

	body, _ := proto.Marshal(&tbproto.BalanceRequest{IdentityId: f.cliPeer[:]})
	resp := rpcSimple(t, conn, tbproto.RpcMethod_RPC_METHOD_BALANCE, body)
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status = %v: %+v", resp.Status, resp.Error)
	}
}

func TestIntegration_RPC_UnknownMethod_Invalid(t *testing.T) {
	f := newFixture(t)
	conn, err := f.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "bye")
	openHB(t, conn)

	resp := rpcSimple(t, conn, tbproto.RpcMethod(99), nil)
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INVALID ||
		resp.Error == nil || resp.Error.Code != "UNKNOWN_METHOD" {
		t.Fatalf("got %+v / %+v", resp.Status, resp.Error)
	}
}

func TestIntegration_RPC_MalformedPayload_Invalid(t *testing.T) {
	f := newFixture(t)
	conn, err := f.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "bye")
	openHB(t, conn)

	// Garbage payload that won't unmarshal as BalanceRequest.
	resp := rpcSimple(t, conn, tbproto.RpcMethod_RPC_METHOD_BALANCE, []byte{0xff, 0xff, 0xff, 0xff})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INVALID {
		t.Fatalf("status = %v", resp.Status)
	}

	// Connection should survive — issue a fresh, valid Balance call.
	body, _ := proto.Marshal(&tbproto.BalanceRequest{IdentityId: f.cliPeer[:]})
	resp2 := rpcSimple(t, conn, tbproto.RpcMethod_RPC_METHOD_BALANCE, body)
	if resp2.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("follow-up balance: %+v", resp2.Error)
	}
}

func TestIntegration_RPC_OversizeFrame_StreamCloses(t *testing.T) {
	f := newFixture(t)
	conn, err := f.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "bye")
	openHB(t, conn)

	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	// Header claims 2 MiB > default MaxFrameSize 1 MiB.
	if _, err := stream.Write([]byte{0x00, 0x20, 0x00, 0x00}); err != nil {
		t.Fatal(err)
	}
	var resp tbproto.RpcResponse
	if err := server.ReadFrame(stream, &resp, 1<<20); err != nil {
		// Stream closed without a response is acceptable per spec.
		t.Logf("server closed stream without response: %v", err)
		return
	}
	if resp.Error == nil || resp.Error.Code != "FRAME" {
		t.Logf("response: %+v", resp.Error)
	}

	// Connection should survive — fresh Balance call should succeed.
	body, _ := proto.Marshal(&tbproto.BalanceRequest{IdentityId: f.cliPeer[:]})
	resp2 := rpcSimple(t, conn, tbproto.RpcMethod_RPC_METHOD_BALANCE, body)
	if resp2.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Errorf("follow-up balance after oversize: %+v", resp2.Error)
	}
}

func TestIntegration_RPC_ConcurrentBalances_25x(t *testing.T) {
	f := newFixture(t)
	conn, err := f.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "bye")
	openHB(t, conn)

	const N = 25
	body, _ := proto.Marshal(&tbproto.BalanceRequest{IdentityId: f.cliPeer[:]})

	var wg sync.WaitGroup
	failures := make(chan string, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := rpcSimple(t, conn, tbproto.RpcMethod_RPC_METHOD_BALANCE, body)
			if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
				failures <- "non-OK"
			}
		}()
	}
	wg.Wait()
	close(failures)
	if len(failures) > 0 {
		t.Fatalf("%d concurrent calls failed", len(failures))
	}
}

func TestIntegration_RPC_AllStubsNotImplemented(t *testing.T) {
	f := newFixture(t)
	conn, err := f.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "bye")
	openHB(t, conn)

	for _, m := range []tbproto.RpcMethod{
		tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
		tbproto.RpcMethod_RPC_METHOD_SETTLE,
		tbproto.RpcMethod_RPC_METHOD_USAGE_REPORT,
		tbproto.RpcMethod_RPC_METHOD_TRANSFER_REQUEST,
	} {
		resp := rpcSimple(t, conn, m, nil)
		if resp.Status != tbproto.RpcStatus_RPC_STATUS_INTERNAL ||
			resp.Error == nil || resp.Error.Code != "NOT_IMPLEMENTED" {
			t.Errorf("method %v: %+v / %+v", m, resp.Status, resp.Error)
		}
	}
}

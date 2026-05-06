package component_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/server"
)

// All-RPC happy-path: 5 real handlers + 4 stub handlers, end-to-end
// over real QUIC with real subsystems.

func TestComponent_Balance_OK(t *testing.T) {
	f := newFixture(t)
	conn := f.dial(t)
	openHeartbeatStream(t, conn)
	waitForPeers(t, f.srv, 1, time.Second)

	body := mustMarshal(t, &tbproto.BalanceRequest{IdentityId: f.cliPeer[:]})
	resp := rpcCall(t, conn, tbproto.RpcMethod_RPC_METHOD_BALANCE, body)

	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status = %v: %+v", resp.Status, resp.Error)
	}
	var snap tbproto.SignedBalanceSnapshot
	if err := proto.Unmarshal(resp.Payload, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Body == nil || !bytes.Equal(snap.Body.IdentityId, f.cliPeer[:]) {
		t.Fatalf("snapshot body = %+v", snap.Body)
	}
	if len(snap.TrackerSig) != 64 {
		t.Errorf("tracker_sig len = %d, want 64", len(snap.TrackerSig))
	}
}

func TestComponent_Advertise_OK(t *testing.T) {
	f := newFixture(t)
	conn := f.dial(t)
	openHeartbeatStream(t, conn)
	waitForPeers(t, f.srv, 1, time.Second)

	body := mustMarshal(t, &tbproto.Advertisement{
		Models:     []string{"claude-opus-4-7"},
		MaxContext: 200_000,
		Available:  true,
		Headroom:   0.5,
		Tiers:      0x1,
	})
	resp := rpcCall(t, conn, tbproto.RpcMethod_RPC_METHOD_ADVERTISE, body)
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status = %v: %+v", resp.Status, resp.Error)
	}
	rec, ok := f.reg.Get(f.cliPeer)
	if !ok {
		t.Fatal("registry has no record after advertise")
	}
	if !rec.Available {
		t.Errorf("registry record not available")
	}
}

func TestComponent_StunAllocate_ReturnsRemoteAddr(t *testing.T) {
	f := newFixture(t)
	conn := f.dial(t)
	openHeartbeatStream(t, conn)
	waitForPeers(t, f.srv, 1, time.Second)

	resp := rpcCall(t, conn, tbproto.RpcMethod_RPC_METHOD_STUN_ALLOCATE, nil)
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status = %v: %+v", resp.Status, resp.Error)
	}
	var out tbproto.StunAllocateResponse
	if err := proto.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.ExternalAddr == "" {
		t.Fatalf("external_addr empty")
	}
}

func TestComponent_TurnRelayOpen_OK(t *testing.T) {
	f := newFixture(t)
	conn := f.dial(t)
	openHeartbeatStream(t, conn)
	waitForPeers(t, f.srv, 1, time.Second)

	body := mustMarshal(t, &tbproto.TurnRelayOpenRequest{
		SessionId: bytes.Repeat([]byte{0xaa}, 16),
	})
	resp := rpcCall(t, conn, tbproto.RpcMethod_RPC_METHOD_TURN_RELAY_OPEN, body)
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status = %v: %+v", resp.Status, resp.Error)
	}
	var out tbproto.TurnRelayOpenResponse
	if err := proto.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Token) == 0 {
		t.Fatal("relay token empty")
	}
	if out.RelayEndpoint == "" {
		t.Fatal("relay_endpoint empty")
	}
}

func TestComponent_Enroll_OK(t *testing.T) {
	f := newFixture(t)
	conn := f.dial(t)
	openHeartbeatStream(t, conn)
	waitForPeers(t, f.srv, 1, time.Second)

	// Build an EnrollRequest where identity_pubkey corresponds to the
	// client's identity (so the handler's defense-in-depth check passes).
	// f.cliPriv is ed25519.PrivateKey; .Public() returns ed25519.PublicKey
	// which is []byte underneath.
	pubKey := []byte(f.cliPriv.Public().(ed25519.PublicKey))

	body := mustMarshal(t, &tbproto.EnrollRequest{
		IdentityPubkey:     pubKey,
		Role:               0x1,
		AccountFingerprint: bytes.Repeat([]byte{0x11}, 32),
		Nonce:              bytes.Repeat([]byte{0x22}, 16),
		ConsumerSig:        bytes.Repeat([]byte{0x33}, 64),
	})

	resp := rpcCall(t, conn, tbproto.RpcMethod_RPC_METHOD_ENROLL, body)
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status = %v: %+v", resp.Status, resp.Error)
	}
	var out tbproto.EnrollResponse
	if err := proto.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.StarterGrantCredits != 1000 {
		t.Errorf("credits = %d", out.StarterGrantCredits)
	}
	if !bytes.Equal(out.IdentityId, f.cliPeer[:]) {
		t.Errorf("identity_id = %x, want %x", out.IdentityId, f.cliPeer)
	}
}

func TestComponent_BrokerRequest_NotImplemented(t *testing.T) {
	stubAssertion(t, tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST)
}

func TestComponent_Settle_NotImplemented(t *testing.T) {
	stubAssertion(t, tbproto.RpcMethod_RPC_METHOD_SETTLE)
}

func TestComponent_UsageReport_NotImplemented(t *testing.T) {
	stubAssertion(t, tbproto.RpcMethod_RPC_METHOD_USAGE_REPORT)
}

func TestComponent_TransferRequest_NotImplemented(t *testing.T) {
	stubAssertion(t, tbproto.RpcMethod_RPC_METHOD_TRANSFER_REQUEST)
}

func stubAssertion(t *testing.T, method tbproto.RpcMethod) {
	t.Helper()
	f := newFixture(t)
	conn := f.dial(t)
	openHeartbeatStream(t, conn)
	waitForPeers(t, f.srv, 1, time.Second)

	resp := rpcCall(t, conn, method, nil)
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INTERNAL {
		t.Fatalf("status = %v", resp.Status)
	}
	if resp.Error == nil || resp.Error.Code != "NOT_IMPLEMENTED" {
		t.Fatalf("error = %+v", resp.Error)
	}
}

func TestComponent_Heartbeat_3Pings_3Pongs(t *testing.T) {
	f := newFixture(t)
	conn := f.dial(t)
	hb := openHeartbeatStream(t, conn)
	waitForPeers(t, f.srv, 1, time.Second)

	for i := uint64(1); i <= 3; i++ {
		if err := server.WriteFrame(hb, &tbproto.HeartbeatPing{Seq: i, T: i * 1000}, 1<<20); err != nil {
			t.Fatal(err)
		}
		var pong tbproto.HeartbeatPong
		if err := server.ReadFrame(hb, &pong, 1<<20); err != nil {
			t.Fatal(err)
		}
		if pong.Seq != i {
			t.Fatalf("pong.Seq = %d, want %d", pong.Seq, i)
		}
	}

	// Each ping should have hit registry.Heartbeat. Verify by querying
	// the registry record for cliPeer (Heartbeat creates a record if
	// none exists per registry.Heartbeat semantics).
	rec, ok := f.reg.Get(f.cliPeer)
	if !ok {
		t.Fatal("registry has no record after 3 heartbeats")
	}
	if rec.LastHeartbeat.IsZero() {
		t.Fatal("LastHeartbeat is zero")
	}
}

func TestComponent_PushOffer_HappyPath(t *testing.T) {
	f := newFixture(t)
	conn := f.dial(t)
	openHeartbeatStream(t, conn)
	waitForPeers(t, f.srv, 1, time.Second)

	// Goroutine: client accepts the server-initiated stream, reads the
	// 1-byte tag + framed OfferPush, replies with OfferDecision{Accept}.
	cliDone := make(chan struct{})
	go func() {
		defer close(cliDone)
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			t.Errorf("AcceptStream: %v", err)
			return
		}
		defer stream.Close()
		var tag [1]byte
		if _, err := stream.Read(tag[:]); err != nil {
			t.Errorf("read tag: %v", err)
			return
		}
		if tag[0] != pushTagOffer {
			t.Errorf("tag = %#x, want %#x", tag[0], pushTagOffer)
			return
		}
		var push tbproto.OfferPush
		if err := server.ReadFrame(stream, &push, 1<<20); err != nil {
			t.Errorf("read push: %v", err)
			return
		}
		_ = server.WriteFrame(stream, &tbproto.OfferDecision{
			Accept:          true,
			EphemeralPubkey: bytes.Repeat([]byte{0xde}, 32),
		}, 1<<20)
	}()

	push := &tbproto.OfferPush{
		ConsumerId:      bytes.Repeat([]byte{0xab}, 32),
		EnvelopeHash:    bytes.Repeat([]byte{0xcd}, 32),
		Model:           "claude-opus-4-7",
		MaxInputTokens:  4096,
		MaxOutputTokens: 1024,
	}
	decisionCh, ok := f.srv.PushOfferTo(f.cliPeer, push)
	if !ok {
		t.Fatal("PushOfferTo returned ok=false")
	}

	select {
	case dec := <-decisionCh:
		if !dec.Accept {
			t.Fatalf("decision = %+v", dec)
		}
		if len(dec.EphemeralPubkey) != 32 {
			t.Errorf("ephemeral_pubkey len = %d", len(dec.EphemeralPubkey))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("decisionCh did not deliver")
	}

	<-cliDone
}

func TestComponent_PushSettlement_HappyPath(t *testing.T) {
	f := newFixture(t)
	conn := f.dial(t)
	openHeartbeatStream(t, conn)
	waitForPeers(t, f.srv, 1, time.Second)

	cliDone := make(chan struct{})
	go func() {
		defer close(cliDone)
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			t.Errorf("AcceptStream: %v", err)
			return
		}
		defer stream.Close()
		var tag [1]byte
		if _, err := stream.Read(tag[:]); err != nil {
			return
		}
		if tag[0] != pushTagSettlement {
			t.Errorf("tag = %#x, want %#x", tag[0], pushTagSettlement)
			return
		}
		var push tbproto.SettlementPush
		if err := server.ReadFrame(stream, &push, 1<<20); err != nil {
			return
		}
		_ = server.WriteFrame(stream, &tbproto.SettleAck{}, 1<<20)
	}()

	push := &tbproto.SettlementPush{
		PreimageHash: bytes.Repeat([]byte{0xee}, 32),
		PreimageBody: []byte("body"),
	}
	ackCh, ok := f.srv.PushSettlementTo(f.cliPeer, push)
	if !ok {
		t.Fatal("PushSettlementTo returned ok=false")
	}

	select {
	case ack, open := <-ackCh:
		if !open {
			t.Fatal("ackCh closed without value")
		}
		if ack == nil {
			t.Fatal("nil SettleAck")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ackCh did not deliver")
	}

	<-cliDone
}

func TestComponent_ConcurrentBalance_25x(t *testing.T) {
	f := newFixture(t)
	conn := f.dial(t)
	openHeartbeatStream(t, conn)
	waitForPeers(t, f.srv, 1, time.Second)

	const N = 25
	body := mustMarshal(t, &tbproto.BalanceRequest{IdentityId: f.cliPeer[:]})

	var wg sync.WaitGroup
	errs := make(chan string, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			resp := rpcCall(t, conn, tbproto.RpcMethod_RPC_METHOD_BALANCE, body)
			if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
				errs <- "non-OK"
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	if len(errs) > 0 {
		t.Fatalf("%d concurrent calls failed", len(errs))
	}
}

//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/api"
	"github.com/token-bay/token-bay/tracker/internal/server"
)

func TestIntegration_Push_Offer_OK(t *testing.T) {
	f := newFixture(t)
	conn, err := f.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "bye")
	openHB(t, conn)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && f.srv.PeerCount() != 1 {
		time.Sleep(10 * time.Millisecond)
	}

	// Client goroutine: accept the server-initiated stream and reply.
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
		if tag[0] != api.PushTagOffer {
			t.Errorf("tag = %#x", tag[0])
			return
		}
		var push tbproto.OfferPush
		if err := server.ReadFrame(stream, &push, 1<<20); err != nil {
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
	case <-time.After(2 * time.Second):
		t.Fatal("decisionCh did not deliver")
	}
	<-cliDone
}

func TestIntegration_Push_Offer_ToUnknownPeer(t *testing.T) {
	f := newFixture(t)
	ch, ok := f.srv.PushOfferTo(ids.IdentityID{0xff}, &tbproto.OfferPush{})
	if ok {
		t.Fatal("ok=true for unknown peer")
	}
	if ch != nil {
		t.Fatal("non-nil channel for unknown peer")
	}
}

func TestIntegration_Push_Offer_AfterShutdown(t *testing.T) {
	f := newFixture(t)
	dl, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = f.srv.Shutdown(dl)

	ch, ok := f.srv.PushOfferTo(f.cliPeer, &tbproto.OfferPush{})
	if ok {
		t.Fatal("ok=true after Shutdown")
	}
	if ch != nil {
		t.Fatal("non-nil channel after Shutdown")
	}
}

func TestIntegration_Push_Offer_ClientNeverReplies_TimeoutReject(t *testing.T) {
	// Tight broker.OfferTimeoutMs in fixture (250ms) so the test runs fast.
	f := newFixtureWithOfferTimeout(t, 250)

	conn, err := f.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "bye")
	openHB(t, conn)
	for f.srv.PeerCount() == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	// Client goroutine: accept stream, read the push, then BLOCK
	// (never reply).
	go func() {
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			return
		}
		defer stream.Close()
		var tag [1]byte
		_, _ = stream.Read(tag[:])
		var push tbproto.OfferPush
		_ = server.ReadFrame(stream, &push, 1<<20)
		// Never write the decision — let server time out.
		select {}
	}()

	push := &tbproto.OfferPush{
		ConsumerId:   bytes.Repeat([]byte{0xab}, 32),
		EnvelopeHash: bytes.Repeat([]byte{0xcd}, 32),
		Model:        "x",
	}
	decisionCh, ok := f.srv.PushOfferTo(f.cliPeer, push)
	if !ok {
		t.Fatal("PushOfferTo returned ok=false")
	}
	select {
	case dec := <-decisionCh:
		if dec.Accept || dec.RejectReason != "timeout" {
			t.Fatalf("decision = %+v, want timeout reject", dec)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("decisionCh did not deliver")
	}
}

func TestIntegration_Push_Settlement_OK(t *testing.T) {
	f := newFixture(t)
	conn, err := f.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "bye")
	openHB(t, conn)
	for f.srv.PeerCount() == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	cliDone := make(chan struct{})
	go func() {
		defer close(cliDone)
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			return
		}
		defer stream.Close()
		var tag [1]byte
		if _, err := stream.Read(tag[:]); err != nil {
			return
		}
		var push tbproto.SettlementPush
		_ = server.ReadFrame(stream, &push, 1<<20)
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
		if !open || ack == nil {
			t.Fatal("ack not delivered")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ackCh did not deliver")
	}
	<-cliDone
}

func TestIntegration_Push_Concurrent_5x(t *testing.T) {
	f := newFixture(t)
	conn, err := f.dial(t)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "bye")
	openHB(t, conn)
	for f.srv.PeerCount() == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	// Launch a stream-acceptor goroutine that handles N pushes.
	const N = 5
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			stream, err := conn.AcceptStream(context.Background())
			if err != nil {
				return
			}
			go func(s interface{ Read([]byte) (int, error); Write([]byte) (int, error); Close() error }) {
				defer s.Close()
				var tag [1]byte
				_, _ = s.Read(tag[:])
				var push tbproto.OfferPush
				_ = server.ReadFrame(s, &push, 1<<20)
				_ = server.WriteFrame(s, &tbproto.OfferDecision{Accept: true, EphemeralPubkey: bytes.Repeat([]byte{0xde}, 32)}, 1<<20)
			}(stream)
		}
	}()

	for i := 0; i < N; i++ {
		ch, ok := f.srv.PushOfferTo(f.cliPeer, &tbproto.OfferPush{
			ConsumerId:   bytes.Repeat([]byte{0xaa}, 32),
			EnvelopeHash: bytes.Repeat([]byte{0xbb}, 32),
			Model:        "x",
		})
		if !ok {
			t.Fatalf("push #%d: PushOfferTo ok=false", i)
		}
		select {
		case dec := <-ch:
			if !dec.Accept {
				t.Errorf("push #%d: got reject %q", i, dec.RejectReason)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("push #%d: no decision", i)
		}
	}
	wg.Wait()
}

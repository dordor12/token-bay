package server

import (
	"context"
	"net/netip"
	"testing"

	quicgo "github.com/quic-go/quic-go"

	"github.com/token-bay/token-bay/shared/ids"
)

// fakeQuicConn implements quicConn for connection-level tests.
type fakeQuicConn struct {
	closes int
	ctx    context.Context
	cancel context.CancelFunc
}

func newFakeQuic() *fakeQuicConn {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeQuicConn{ctx: ctx, cancel: cancel}
}

func (f *fakeQuicConn) OpenStreamSync(_ context.Context) (*quicgo.Stream, error) { return nil, nil }
func (f *fakeQuicConn) AcceptStream(_ context.Context) (*quicgo.Stream, error)   { return nil, nil }
func (f *fakeQuicConn) Context() context.Context                                 { return f.ctx }
func (f *fakeQuicConn) CloseWithError(quicgo.ApplicationErrorCode, string) error {
	f.closes++
	f.cancel()
	return nil
}

func TestConnection_New_FieldsPopulated(t *testing.T) {
	qc := newFakeQuic()
	defer qc.cancel()

	id := ids.IdentityID{1, 2, 3}
	addr := netip.MustParseAddrPort("127.0.0.1:55001")
	c := newConnection(context.Background(), qc, id, addr)

	if c.PeerID() != id {
		t.Errorf("peer id = %x, want %x", c.PeerID(), id)
	}
	if c.RemoteAddr() != addr {
		t.Errorf("remote addr = %v, want %v", c.RemoteAddr(), addr)
	}
	select {
	case <-c.Done():
		t.Fatal("Done fired before Close")
	default:
	}
}

func TestConnection_Close_CancelsCtx(t *testing.T) {
	qc := newFakeQuic()
	c := newConnection(context.Background(), qc, ids.IdentityID{}, netip.AddrPort{})
	c.Close()

	select {
	case <-c.Done():
	default:
		t.Fatal("Close did not cancel ctx")
	}
	if qc.closes != 1 {
		t.Fatalf("CloseWithError called %d times, want 1", qc.closes)
	}
}

func TestConnection_Close_Idempotent(t *testing.T) {
	qc := newFakeQuic()
	c := newConnection(context.Background(), qc, ids.IdentityID{}, netip.AddrPort{})
	c.Close()
	c.Close()
	c.Close()

	if qc.closes != 1 {
		t.Fatalf("CloseWithError called %d times across 3 Close()s, want 1", qc.closes)
	}
}

func TestConnection_StreamSeq_Atomic(t *testing.T) {
	qc := newFakeQuic()
	c := newConnection(context.Background(), qc, ids.IdentityID{}, netip.AddrPort{})
	defer c.Close()

	if got := c.streamSeq.Add(1); got != 1 {
		t.Fatalf("streamSeq.Add = %d, want 1", got)
	}
	if got := c.streamSeq.Add(1); got != 2 {
		t.Fatalf("streamSeq.Add = %d, want 2", got)
	}
}

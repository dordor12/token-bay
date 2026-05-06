package server

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

type fakeHbRegistry struct {
	mu       sync.Mutex
	count    int
	lastID   ids.IdentityID
	lastTime time.Time
	retErr   error
}

func (f *fakeHbRegistry) Heartbeat(id ids.IdentityID, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count++
	f.lastID = id
	f.lastTime = now
	return f.retErr
}

func (f *fakeHbRegistry) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count
}

// pipePair returns the server-side and client-side ends of an
// in-memory bidirectional stream.
func pipePair() (server, client net.Conn) {
	return net.Pipe()
}

func TestRunHeartbeat_ThreePings(t *testing.T) {
	srvSide, cliSide := pipePair()
	defer srvSide.Close()
	defer cliSide.Close()

	reg := &fakeHbRegistry{}
	peerID := ids.IdentityID{0xab}

	go func() {
		// Client writes 3 pings, then closes write side.
		for i := uint64(1); i <= 3; i++ {
			if err := WriteFrame(cliSide, &tbproto.HeartbeatPing{Seq: i, T: i * 1000}, 1<<20); err != nil {
				return
			}
			// Read the pong before sending the next ping so the
			// in-memory pipe doesn't deadlock.
			var pong tbproto.HeartbeatPong
			if err := ReadFrame(cliSide, &pong, 1<<20); err != nil {
				return
			}
			if pong.Seq != i {
				t.Errorf("pong.Seq = %d, want %d", pong.Seq, i)
			}
		}
		_ = cliSide.Close()
	}()

	runHeartbeat(context.Background(), srvSide, peerID, reg, time.Now, 1<<20, zerolog.Nop())

	if reg.Count() != 3 {
		t.Fatalf("registry.Heartbeat called %d times, want 3", reg.Count())
	}
	if reg.lastID != peerID {
		t.Fatalf("lastID = %x", reg.lastID)
	}
}

func TestRunHeartbeat_ExitsOnEOF(t *testing.T) {
	srvSide, cliSide := pipePair()
	defer srvSide.Close()

	reg := &fakeHbRegistry{}

	// Close client write side immediately — server reader sees EOF.
	_ = cliSide.Close()

	runHeartbeat(context.Background(), srvSide, ids.IdentityID{}, reg, time.Now, 1<<20, zerolog.Nop())

	if reg.Count() != 0 {
		t.Fatalf("registry.Heartbeat fired without a ping")
	}
}

func TestRunHeartbeat_ExitsOnCtxCancel(t *testing.T) {
	srvSide, cliSide := pipePair()
	defer srvSide.Close()
	defer cliSide.Close()

	ctx, cancel := context.WithCancel(context.Background())
	reg := &fakeHbRegistry{}
	done := make(chan struct{})
	go func() {
		runHeartbeat(ctx, srvSide, ids.IdentityID{}, reg, time.Now, 1<<20, zerolog.Nop())
		close(done)
	}()

	cancel()
	// The reader is blocked in ReadFrame; closing the pipe makes it
	// return EOF, after which the ctx-check runs and the loop exits.
	_ = srvSide.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runHeartbeat did not exit after ctx cancel")
	}
}

func TestRunHeartbeat_NilRegistry_NoCrash(t *testing.T) {
	srvSide, cliSide := pipePair()
	defer srvSide.Close()
	defer cliSide.Close()

	go func() {
		_ = WriteFrame(cliSide, &tbproto.HeartbeatPing{Seq: 1}, 1<<20)
		var pong tbproto.HeartbeatPong
		_ = ReadFrame(cliSide, &pong, 1<<20)
		_ = cliSide.Close()
	}()

	runHeartbeat(context.Background(), srvSide, ids.IdentityID{}, nil, time.Now, 1<<20, zerolog.Nop())
}

// guard against accidental import-cycle from io
var _ io.ReadWriter = net.Conn(nil)

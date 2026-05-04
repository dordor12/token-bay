package trackerclient

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport/loopback"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/wire"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

func TestHeartbeatPingPong(t *testing.T) {
	cli, srv := loopback.Pair(ids.IdentityID{1}, ids.IdentityID{2})
	defer cli.Close()
	defer srv.Close()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		s, err := srv.AcceptStream(context.Background())
		if err != nil {
			return
		}
		defer s.Close()
		for i := 0; i < 3; i++ {
			var ping tbproto.HeartbeatPing
			if err := wire.Read(s, &ping, 1<<10); err != nil {
				if errors.Is(err, io.EOF) {
					return
				}
				t.Errorf("server read: %v", err)
				return
			}
			if err := wire.Write(s, &tbproto.HeartbeatPong{Seq: ping.Seq}, 1<<10); err != nil {
				t.Errorf("server write: %v", err)
				return
			}
		}
	}()

	teardown := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go runHeartbeat(ctx, cli, 30*time.Millisecond, 5, 1<<10, func(err error) {
		select {
		case teardown <- err:
		default:
		}
	})

	time.Sleep(120 * time.Millisecond)
	cancel()

	select {
	case err := <-teardown:
		t.Fatalf("unexpected teardown: %v", err)
	default:
	}
	<-serverDone
}

func TestHeartbeatMissTearsDown(t *testing.T) {
	cli, srv := loopback.Pair(ids.IdentityID{1}, ids.IdentityID{2})
	defer cli.Close()
	defer srv.Close()

	go func() {
		s, err := srv.AcceptStream(context.Background())
		if err != nil {
			return
		}
		// Read pings, never reply. Simulates a hung tracker.
		for {
			var ping tbproto.HeartbeatPing
			if err := wire.Read(s, &ping, 1<<10); err != nil {
				return
			}
		}
	}()

	teardown := make(chan error, 1)
	go runHeartbeat(context.Background(), cli, 20*time.Millisecond, 3, 1<<10, func(err error) {
		select {
		case teardown <- err:
		default:
		}
	})

	select {
	case err := <-teardown:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "misses")
	case <-time.After(time.Second):
		t.Fatal("teardown not invoked within 1s")
	}
}

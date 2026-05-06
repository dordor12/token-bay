package trackerclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/wire"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// runHeartbeat opens the dedicated heartbeat stream as the first
// client-initiated bidi stream after handshake. Sends a ping every
// period, expects a pong within the same period; tearDown is invoked
// (concurrency-safe) after misses consecutive missing pongs.
func runHeartbeat(
	ctx context.Context,
	conn transport.Conn,
	period time.Duration,
	misses int,
	maxFrameSize int,
	tearDown func(error),
) {
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		tearDown(fmt.Errorf("trackerclient: open heartbeat stream: %w", err))
		return
	}
	defer stream.Close()

	var lastPong int64 // atomic; unix-nano of last pong received
	atomic.StoreInt64(&lastPong, time.Now().UnixNano())

	go func() {
		for {
			var pong tbproto.HeartbeatPong
			err := wire.Read(stream, &pong, maxFrameSize)
			if err != nil {
				if errors.Is(err, io.EOF) || ctx.Err() != nil {
					return
				}
				tearDown(fmt.Errorf("trackerclient: heartbeat read: %w", err))
				return
			}
			atomic.StoreInt64(&lastPong, time.Now().UnixNano())
		}
	}()

	seq := uint64(0)
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-conn.Done():
			return
		case now := <-ticker.C:
			seq++
			//nolint:gosec // unix-millis fits in uint64 indefinitely
			if err := wire.Write(stream, &tbproto.HeartbeatPing{Seq: seq, T: uint64(now.UnixMilli())}, maxFrameSize); err != nil {
				tearDown(fmt.Errorf("trackerclient: heartbeat write: %w", err))
				return
			}
			since := time.Since(time.Unix(0, atomic.LoadInt64(&lastPong)))
			if since > time.Duration(misses)*period {
				tearDown(fmt.Errorf("trackerclient: heartbeat: %d misses (last pong %s ago)", misses, since))
				return
			}
		}
	}
}

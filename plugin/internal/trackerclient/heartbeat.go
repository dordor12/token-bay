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

// openHeartbeatStream opens the dedicated heartbeat stream. The tracker
// treats the FIRST client-initiated bidi stream it accepts as the
// heartbeat stream, so the supervisor MUST call this synchronously
// before signaling PhaseConnected — otherwise an application RPC
// unblocked by WaitConnected can open its stream first and be consumed
// by the tracker's heartbeat handler (and the real heartbeat stream be
// dispatched as an RPC).
func openHeartbeatStream(ctx context.Context, conn transport.Conn) (transport.Stream, error) {
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("trackerclient: open heartbeat stream: %w", err)
	}
	return stream, nil
}

// runHeartbeatLoop drives ping/pong on an already-open heartbeat
// stream. Sends a ping every period, expects a pong within the same
// period; tearDown is invoked (concurrency-safe) after misses
// consecutive missing pongs. Closes the stream on return.
func runHeartbeatLoop(
	ctx context.Context,
	conn transport.Conn,
	stream transport.Stream,
	period time.Duration,
	misses int,
	maxFrameSize int,
	tearDown func(error),
) {
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

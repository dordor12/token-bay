package server

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// heartbeatRegistry is the slice of registry the heartbeat reader uses.
type heartbeatRegistry interface {
	Heartbeat(id ids.IdentityID, now time.Time) error
}

// runHeartbeat reads bare HeartbeatPing frames off stream and writes
// HeartbeatPong replies. Calls reg.Heartbeat(peerID, now) on each ping.
// Exits when ctx cancels, the stream EOFs, or a frame error occurs.
//
// Wire reality: the merged plugin client writes raw HeartbeatPing on
// the heartbeat stream — NOT RpcRequest{method=0,payload=HeartbeatPing}.
// See plan §B (wire-reality reconciliation).
func runHeartbeat(
	ctx context.Context,
	stream io.ReadWriter,
	peerID ids.IdentityID,
	reg heartbeatRegistry,
	now func() time.Time,
	maxFrameSize int,
	log zerolog.Logger,
) {
	for {
		if ctx.Err() != nil {
			return
		}
		var ping tbproto.HeartbeatPing
		err := ReadFrame(stream, &ping, maxFrameSize)
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			log.Warn().Err(err).Hex("peer", peerID[:]).Msg("server: heartbeat read")
			return
		}
		if reg != nil {
			if err := reg.Heartbeat(peerID, now()); err != nil {
				log.Warn().Err(err).Hex("peer", peerID[:]).Msg("server: registry.Heartbeat")
			}
		}
		if err := WriteFrame(stream, &tbproto.HeartbeatPong{Seq: ping.Seq}, maxFrameSize); err != nil {
			log.Warn().Err(err).Hex("peer", peerID[:]).Msg("server: heartbeat write")
			return
		}
	}
}

package server

import (
	"context"
	"time"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/api"
)

// PushOfferTo enqueues an OfferPush to the given identity.
//
// Returns (nil, false) if the peer is not currently connected or the
// server is not running. Returns (decisionCh, true) otherwise; the
// channel receives exactly one *OfferDecision (or a Reject value on
// timeout / drop) and then closes.
func (s *Server) PushOfferTo(id ids.IdentityID, push *tbproto.OfferPush) (<-chan *tbproto.OfferDecision, bool) {
	s.mu.Lock()
	if !s.running || s.stopped {
		s.mu.Unlock()
		return nil, false
	}
	c, ok := s.connsByPeer[id]
	s.mu.Unlock()
	if !ok {
		return nil, false
	}
	out := make(chan *tbproto.OfferDecision, 1)
	go s.runOfferPush(c, push, out)
	return out, true
}

// runOfferPush opens a server-initiated stream, writes tag(0x01) +
// framed OfferPush, awaits the consumer's framed OfferDecision, sends
// it on out, closes out.
func (s *Server) runOfferPush(c *Connection, push *tbproto.OfferPush, out chan<- *tbproto.OfferDecision) {
	defer close(out)

	maxFrame := s.deps.Config.Server.MaxFrameSize
	timeoutMs := s.deps.Config.Broker.OfferTimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 5_000
	}
	ctx, cancel := context.WithTimeout(c.ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	stream, err := c.conn.OpenStreamSync(ctx)
	if err != nil {
		out <- &tbproto.OfferDecision{Accept: false, RejectReason: rejectReason(ctx, err)}
		return
	}
	defer stream.Close()

	// Bound the entire I/O dance by the same deadline. quic-go.Stream
	// honors SetReadDeadline / SetWriteDeadline; without these the
	// ReadFrame for OfferDecision would block past ctx expiry.
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	_ = stream.SetWriteDeadline(deadline)
	_ = stream.SetReadDeadline(deadline)

	if _, err := stream.Write([]byte{api.PushTagOffer}); err != nil {
		out <- &tbproto.OfferDecision{Accept: false, RejectReason: rejectReason(ctx, err)}
		return
	}
	if err := WriteFrame(stream, push, maxFrame); err != nil {
		out <- &tbproto.OfferDecision{Accept: false, RejectReason: rejectReason(ctx, err)}
		return
	}

	var dec tbproto.OfferDecision
	if err := ReadFrame(stream, &dec, maxFrame); err != nil {
		out <- &tbproto.OfferDecision{Accept: false, RejectReason: timeoutOrLost(deadline, err)}
		return
	}
	out <- &dec
}

// timeoutOrLost classifies a stream-IO error: if the deadline has
// passed, it's a timeout; otherwise the connection is gone.
func timeoutOrLost(deadline time.Time, err error) string {
	if time.Now().After(deadline) {
		return "timeout"
	}
	if err != nil {
		return "connection_lost"
	}
	return "connection_lost"
}

// rejectReason classifies an error from the push pipeline. ctx
// timeout → "timeout"; everything else → "connection_lost".
func rejectReason(ctx context.Context, err error) string {
	if ctx.Err() == context.DeadlineExceeded {
		return "timeout"
	}
	if err != nil {
		return "connection_lost"
	}
	return "connection_lost"
}

package server

import (
	"context"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/api"
)

// PushSettlementTo mirrors PushOfferTo for settlement pushes. The
// channel receives a *SettleAck on success; on timeout / drop /
// shutdown the channel closes without delivering a value (the broker
// distinguishes via "ok" from the receive operator).
func (s *Server) PushSettlementTo(id ids.IdentityID, push *tbproto.SettlementPush) (<-chan *tbproto.SettleAck, bool) {
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
	out := make(chan *tbproto.SettleAck, 1)
	go s.runSettlementPush(c, push, out)
	return out, true
}

func (s *Server) runSettlementPush(c *Connection, push *tbproto.SettlementPush, out chan<- *tbproto.SettleAck) {
	defer close(out)

	maxFrame := s.deps.Config.Server.MaxFrameSize
	timeoutS := s.deps.Config.Settlement.SettlementTimeoutS
	if timeoutS <= 0 {
		timeoutS = 30
	}
	ctx, cancel := context.WithTimeout(c.ctx, time.Duration(timeoutS)*time.Second)
	defer cancel()

	stream, err := c.conn.OpenStreamSync(ctx)
	if err != nil {
		return // close-without-value drain reject
	}
	defer stream.Close()

	deadline := time.Now().Add(time.Duration(timeoutS) * time.Second)
	_ = stream.SetWriteDeadline(deadline)
	_ = stream.SetReadDeadline(deadline)

	if _, err := stream.Write([]byte{api.PushTagSettlement}); err != nil {
		return
	}
	if err := WriteFrame(stream, push, maxFrame); err != nil {
		return
	}

	var ack tbproto.SettleAck
	if err := ReadFrame(stream, &ack, maxFrame); err != nil {
		return
	}
	out <- &ack
}

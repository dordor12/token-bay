package server

import (
	"context"

	quicgo "github.com/quic-go/quic-go"
)

// handleRPCStream is wired in Phase 7. The Phase 5 placeholder closes
// the stream so the connection doesn't leak resources.
func (s *Server) handleRPCStream(_ context.Context, stream *quicgo.Stream, _ *Connection) {
	if stream != nil {
		_ = stream.Close()
	}
}

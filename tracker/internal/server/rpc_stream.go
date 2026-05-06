package server

import (
	"context"
	"errors"
	"io"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/api"
)

// rpcStream is the slice of *quicgo.Stream the acceptor needs.
// Satisfied by *quicgo.Stream and by net.Pipe halves for tests.
//
// quic-go's Stream.Close() performs a write-side half-close; the read
// side continues until the peer half-closes. So a single Close() is
// the correct end-of-response signal — no separate CloseWrite is
// needed (or available on quic-go).
type rpcStream interface {
	io.Reader
	io.Writer
	Close() error
}

// handleRPCStream reads one framed RpcRequest, dispatches it via
// the api.Router, writes one framed RpcResponse, half-closes the
// write side. Lifetime is bounded by ctx (the connection's serverCtx).
//
// On read failure: write an INVALID/FRAME response if possible, then
// close. On dispatcher panic: recover and write INTERNAL/PANIC.
func (s *Server) handleRPCStream(ctx context.Context, stream rpcStream, c *Connection) {
	defer stream.Close()
	s.wg.Add(1)
	defer s.wg.Done()

	maxFrame := s.deps.Config.Server.MaxFrameSize

	var req tbproto.RpcRequest
	if err := ReadFrame(stream, &req, maxFrame); err != nil {
		// Truncated header: caller already gone; nothing to write.
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return
		}
		// Oversize / unmarshal-failure: surface as INVALID/FRAME so the
		// client gets a typed reply on this stream before it closes.
		_ = WriteFrame(stream, &tbproto.RpcResponse{
			Status: tbproto.RpcStatus_RPC_STATUS_INVALID,
			Error:  &tbproto.RpcError{Code: "FRAME", Message: err.Error()},
		}, maxFrame)
		return
	}

	rc := &api.RequestCtx{
		PeerID:     c.peerID,
		RemoteAddr: c.remoteAddr,
		Now:        s.deps.Now(),
		Logger:     s.deps.Logger.With().Hex("peer", c.peerID[:]).Logger(),
	}

	resp := func() (out *tbproto.RpcResponse) {
		defer func() {
			if r := recover(); r != nil {
				s.deps.Logger.Error().
					Interface("panic", r).
					Hex("peer", c.peerID[:]).
					Msg("server: handler panic")
				out = &tbproto.RpcResponse{
					Status: tbproto.RpcStatus_RPC_STATUS_INTERNAL,
					Error:  &tbproto.RpcError{Code: "PANIC", Message: "handler panicked"},
				}
			}
		}()
		return s.deps.API.Dispatch(ctx, rc, &req)
	}()

	if err := WriteFrame(stream, resp, maxFrame); err != nil {
		s.deps.Logger.Warn().Err(err).Hex("peer", c.peerID[:]).Msg("server: write response")
	}
	// stream.Close() (deferred above) performs the write-side half-close.
	// quic-go has no separate CloseWrite — Close already half-closes.
}

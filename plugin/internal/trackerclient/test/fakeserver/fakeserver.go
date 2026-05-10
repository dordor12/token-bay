// Package fakeserver implements just enough of the tracker side of the
// wire protocol to drive trackerclient tests. It reads RpcRequests off a
// loopback transport and dispatches to per-method handlers; default
// handlers return RPC_STATUS_OK with an empty payload.
package fakeserver

import (
	"context"
	"errors"
	"io"

	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/wire"
	fed "github.com/token-bay/token-bay/shared/federation"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// Handler signature: receive a per-method request, return a status + an
// optional response payload.
type Handler func(ctx context.Context, req proto.Message) (tbproto.RpcStatus, proto.Message, *tbproto.RpcError)

// Server is the in-process tracker test fake.
type Server struct {
	Conn        transport.Conn
	MaxFrameSz  int
	Handlers    map[tbproto.RpcMethod]Handler
	PayloadType map[tbproto.RpcMethod]func() proto.Message
}

// New constructs a Server bound to conn.
func New(conn transport.Conn) *Server {
	return &Server{
		Conn:       conn,
		MaxFrameSz: 1 << 20,
		Handlers:   map[tbproto.RpcMethod]Handler{},
		PayloadType: map[tbproto.RpcMethod]func() proto.Message{
			tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST:   func() proto.Message { return &tbproto.EnvelopeSigned{} },
			tbproto.RpcMethod_RPC_METHOD_SETTLE:           func() proto.Message { return &tbproto.SettleRequest{} },
			tbproto.RpcMethod_RPC_METHOD_ENROLL:           func() proto.Message { return &tbproto.EnrollRequest{} },
			tbproto.RpcMethod_RPC_METHOD_BALANCE:          func() proto.Message { return &tbproto.BalanceRequest{} },
			tbproto.RpcMethod_RPC_METHOD_USAGE_REPORT:     func() proto.Message { return &tbproto.UsageReport{} },
			tbproto.RpcMethod_RPC_METHOD_ADVERTISE:        func() proto.Message { return &tbproto.Advertisement{} },
			tbproto.RpcMethod_RPC_METHOD_TRANSFER_REQUEST: func() proto.Message { return &tbproto.TransferRequest{} },
			tbproto.RpcMethod_RPC_METHOD_STUN_ALLOCATE:    func() proto.Message { return &tbproto.StunAllocateRequest{} },
			tbproto.RpcMethod_RPC_METHOD_TURN_RELAY_OPEN:  func() proto.Message { return &tbproto.TurnRelayOpenRequest{} },
		},
	}
}

// Run reads one stream at a time off the conn until ctx is cancelled or
// the conn closes. Each stream gets one request → one response.
func (s *Server) Run(ctx context.Context) error {
	for {
		stream, err := s.Conn.AcceptStream(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handle(ctx, stream)
	}
}

func (s *Server) handle(ctx context.Context, stream transport.Stream) {
	defer stream.Close()
	var req tbproto.RpcRequest
	if err := wire.Read(stream, &req, s.MaxFrameSz); err != nil {
		return
	}

	typ, ok := s.PayloadType[req.Method]
	if !ok {
		s.respond(stream, tbproto.RpcStatus_RPC_STATUS_INTERNAL, nil, &tbproto.RpcError{
			Code: "unknown_method", Message: req.Method.String(),
		})
		return
	}
	payload := typ()
	if err := proto.Unmarshal(req.Payload, payload); err != nil {
		s.respond(stream, tbproto.RpcStatus_RPC_STATUS_INVALID, nil, &tbproto.RpcError{
			Code: "bad_payload", Message: err.Error(),
		})
		return
	}

	h, ok := s.Handlers[req.Method]
	if !ok {
		// Default handler: OK with empty payload.
		s.respond(stream, tbproto.RpcStatus_RPC_STATUS_OK, nil, nil)
		return
	}
	status, resp, rerr := h(ctx, payload)
	s.respond(stream, status, resp, rerr)
}

// PushOffer opens a server-initiated stream and writes the offer-class
// tag (0x01) + a single OfferPush. Returns the matching OfferDecision.
func (s *Server) PushOffer(ctx context.Context, push *tbproto.OfferPush) (*tbproto.OfferDecision, error) {
	stream, err := s.Conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	if _, err := stream.Write([]byte{0x01}); err != nil {
		return nil, err
	}
	if err := wire.Write(stream, push, s.MaxFrameSz); err != nil {
		return nil, err
	}
	if err := stream.CloseWrite(); err != nil {
		return nil, err
	}
	var dec tbproto.OfferDecision
	if err := wire.Read(stream, &dec, s.MaxFrameSz); err != nil {
		return nil, err
	}
	return &dec, nil
}

// PushPeerExchange opens a server-initiated stream and writes the
// peer-exchange-class tag (0x03) + a single federation.Envelope frame.
// Peer-exchange is fire-and-forget: this method does NOT wait for any
// reply from the client. Returns when the framed envelope has been
// written and the stream's write half closed.
func (s *Server) PushPeerExchange(ctx context.Context, env *fed.Envelope) error {
	stream, err := s.Conn.OpenStreamSync(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()
	if _, err := stream.Write([]byte{0x03}); err != nil {
		return err
	}
	if err := wire.Write(stream, env, s.MaxFrameSz); err != nil {
		return err
	}
	return stream.CloseWrite()
}

// PushSettlement opens a server-initiated stream and writes the
// settlement-class tag (0x02) + a single SettlementPush. Returns when
// the client acks via SettleAck.
func (s *Server) PushSettlement(ctx context.Context, push *tbproto.SettlementPush) error {
	stream, err := s.Conn.OpenStreamSync(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()
	if _, err := stream.Write([]byte{0x02}); err != nil {
		return err
	}
	if err := wire.Write(stream, push, s.MaxFrameSz); err != nil {
		return err
	}
	if err := stream.CloseWrite(); err != nil {
		return err
	}
	var ack tbproto.SettleAck
	return wire.Read(stream, &ack, s.MaxFrameSz)
}

func (s *Server) respond(stream transport.Stream, status tbproto.RpcStatus, payload proto.Message, rerr *tbproto.RpcError) {
	out := &tbproto.RpcResponse{Status: status, Error: rerr}
	if payload != nil {
		b, err := proto.Marshal(payload)
		if err != nil {
			return
		}
		out.Payload = b
	}
	_ = wire.Write(stream, out, s.MaxFrameSz)
}

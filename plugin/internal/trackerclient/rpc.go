package trackerclient

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/wire"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// callUnary opens a fresh stream, writes one RpcRequest framed, reads one
// RpcResponse framed, and returns the per-method response payload via dst.
//
// Concurrency: safe — each call opens its own stream.
func (c *Client) callUnary(ctx context.Context, method tbproto.RpcMethod, payload proto.Message, dst proto.Message) error {
	conn, err := c.connect(ctx)
	if err != nil {
		return err
	}

	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		if isConnDead(conn) {
			return ErrConnectionLost
		}
		return fmt.Errorf("trackerclient: open stream: %w", err)
	}
	defer stream.Close()

	req, err := wire.MarshalRequest(method, payload)
	if err != nil {
		return err
	}
	if err := tbproto.ValidateRPCRequest(req); err != nil {
		return fmt.Errorf("trackerclient: validate request: %w", err)
	}
	if err := wire.Write(stream, req, c.cfg.MaxFrameSize); err != nil {
		return fmt.Errorf("trackerclient: write request: %w", err)
	}
	_ = stream.CloseWrite()

	var resp tbproto.RpcResponse
	if err := wire.Read(stream, &resp, c.cfg.MaxFrameSize); err != nil {
		if isConnDead(conn) {
			return ErrConnectionLost
		}
		return fmt.Errorf("trackerclient: read response: %w", err)
	}
	if err := tbproto.ValidateRPCResponse(&resp); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
	if err := statusToErr(resp.Status, resp.Error); err != nil {
		return err
	}
	if err := wire.UnmarshalResponse(&resp, dst); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
	return nil
}

func isConnDead(conn interface{ Done() <-chan struct{} }) bool {
	select {
	case <-conn.Done():
		return true
	default:
		return false
	}
}

// BrokerRequest sends an EnvelopeSigned and returns the seeder assignment.
func (c *Client) BrokerRequest(ctx context.Context, env *tbproto.EnvelopeSigned) (*BrokerResponse, error) {
	if env == nil {
		return nil, fmt.Errorf("%w: nil envelope", ErrInvalidResponse)
	}
	var resp tbproto.BrokerResponse
	if err := c.callUnary(ctx, tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST, env, &resp); err != nil {
		return nil, err
	}
	return &BrokerResponse{
		SeederAddr:       string(resp.SeederAddr),
		SeederPubkey:     resp.SeederPubkey,
		ReservationToken: resp.ReservationToken,
	}, nil
}

// Settle sends the consumer's signature over a finalized entry preimage.
func (c *Client) Settle(ctx context.Context, preimageHash, sig []byte) error {
	if len(preimageHash) != 32 {
		return fmt.Errorf("%w: preimageHash len=%d, want 32", ErrInvalidResponse, len(preimageHash))
	}
	req := &tbproto.SettleRequest{PreimageHash: preimageHash, ConsumerSig: sig}
	var ack tbproto.SettleAck
	return c.callUnary(ctx, tbproto.RpcMethod_RPC_METHOD_SETTLE, req, &ack)
}

// Enroll requests an identity binding from the tracker.
func (c *Client) Enroll(ctx context.Context, r *EnrollRequest) (*EnrollResponse, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: nil EnrollRequest", ErrInvalidResponse)
	}
	req := &tbproto.EnrollRequest{
		IdentityPubkey:     r.IdentityPubkey,
		Role:               r.Role,
		AccountFingerprint: r.AccountFingerprint[:],
		Nonce:              r.Nonce[:],
		ConsumerSig:        r.Sig,
	}
	var resp tbproto.EnrollResponse
	if err := c.callUnary(ctx, tbproto.RpcMethod_RPC_METHOD_ENROLL, req, &resp); err != nil {
		return nil, err
	}
	out := &EnrollResponse{
		StarterGrantCredits:   resp.StarterGrantCredits,
		StarterGrantEntryBlob: resp.StarterGrantEntry,
	}
	copy(out.IdentityID[:], resp.IdentityId)
	return out, nil
}

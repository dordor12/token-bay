package trackerclient

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/wire"
	"github.com/token-bay/token-bay/shared/ids"
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

// Balance fetches a fresh signed balance snapshot from the tracker.
// Bypasses the cache; callers usually want BalanceCached instead.
func (c *Client) Balance(ctx context.Context, id ids.IdentityID) (*tbproto.SignedBalanceSnapshot, error) {
	req := &tbproto.BalanceRequest{IdentityId: id[:]}
	var resp tbproto.SignedBalanceSnapshot
	if err := c.callUnary(ctx, tbproto.RpcMethod_RPC_METHOD_BALANCE, req, &resp); err != nil {
		return nil, err
	}
	if resp.Body == nil {
		return nil, fmt.Errorf("%w: balance response missing body", ErrInvalidResponse)
	}
	return &resp, nil
}

// BalanceCached returns a cached snapshot if fresh, otherwise refreshes
// via Balance(). Concurrent stale callers coalesce on the same fetch.
func (c *Client) BalanceCached(ctx context.Context, id ids.IdentityID) (*tbproto.SignedBalanceSnapshot, error) {
	return c.cache.Get(ctx, id, c.Balance)
}

// UsageReport is sent by the seeder after a served request completes.
func (c *Client) UsageReport(ctx context.Context, ur *UsageReport) error {
	if ur == nil {
		return fmt.Errorf("%w: nil UsageReport", ErrInvalidResponse)
	}
	rid, _ := ur.RequestID.MarshalBinary()
	req := &tbproto.UsageReport{
		RequestId:    rid,
		InputTokens:  ur.InputTokens,
		OutputTokens: ur.OutputTokens,
		Model:        ur.Model,
		SeederSig:    ur.SeederSig,
	}
	var ack tbproto.UsageAck
	return c.callUnary(ctx, tbproto.RpcMethod_RPC_METHOD_USAGE_REPORT, req, &ack)
}

// Advertise updates the seeder's availability + capabilities.
func (c *Client) Advertise(ctx context.Context, ad *Advertisement) error {
	if ad == nil {
		return fmt.Errorf("%w: nil Advertisement", ErrInvalidResponse)
	}
	req := &tbproto.Advertisement{
		Models:     ad.Models,
		MaxContext: ad.MaxContext,
		Available:  ad.Available,
		Headroom:   ad.Headroom,
		Tiers:      ad.Tiers,
	}
	var ack tbproto.AdvertiseAck
	return c.callUnary(ctx, tbproto.RpcMethod_RPC_METHOD_ADVERTISE, req, &ack)
}

// TransferRequest moves credits between regions.
func (c *Client) TransferRequest(ctx context.Context, tr *TransferRequest) (*TransferProof, error) {
	if tr == nil {
		return nil, fmt.Errorf("%w: nil TransferRequest", ErrInvalidResponse)
	}
	req := &tbproto.TransferRequest{
		IdentityId: tr.IdentityID[:],
		Amount:     tr.Amount,
		DestRegion: tr.DestRegion,
		Nonce:      tr.Nonce[:],
	}
	var resp tbproto.TransferProof
	if err := c.callUnary(ctx, tbproto.RpcMethod_RPC_METHOD_TRANSFER_REQUEST, req, &resp); err != nil {
		return nil, err
	}
	out := &TransferProof{
		SourceSeq:  resp.SourceSeq,
		TrackerSig: resp.TrackerSig,
	}
	copy(out.SourceChainTipHash[:], resp.SourceChainTipHash)
	return out, nil
}

// StunAllocate asks the tracker to reflect the client's external address.
func (c *Client) StunAllocate(ctx context.Context) (netip.AddrPort, error) {
	var resp tbproto.StunAllocateResponse
	if err := c.callUnary(ctx, tbproto.RpcMethod_RPC_METHOD_STUN_ALLOCATE, &tbproto.StunAllocateRequest{}, &resp); err != nil {
		return netip.AddrPort{}, err
	}
	out, err := netip.ParseAddrPort(resp.ExternalAddr)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("%w: bad external_addr %q", ErrInvalidResponse, resp.ExternalAddr)
	}
	return out, nil
}

// TurnRelayOpen requests a TURN-style relay allocation.
func (c *Client) TurnRelayOpen(ctx context.Context, sessionID uuid.UUID) (*RelayHandle, error) {
	sid, _ := sessionID.MarshalBinary()
	var resp tbproto.TurnRelayOpenResponse
	if err := c.callUnary(ctx, tbproto.RpcMethod_RPC_METHOD_TURN_RELAY_OPEN, &tbproto.TurnRelayOpenRequest{SessionId: sid}, &resp); err != nil {
		return nil, err
	}
	return &RelayHandle{Endpoint: resp.RelayEndpoint, Token: resp.Token}, nil
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

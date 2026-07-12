//go:build e2e || perf

package driver

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net"
	"time"

	quicgo "github.com/quic-go/quic-go"
	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/api"
)

// This file extends RPCClient with the client-side surface the perf
// harness's simulated participants need beyond one-shot scenario RPCs:
// dialing thousands of connections through a shared UDP socket,
// accepting server-initiated push streams (offers to seeders,
// settlements to consumers), and sending heartbeat pings on the held
// heartbeat stream.

// PushTagOffer / PushTagSettlement re-export the server's push-stream
// tag bytes so perf loadgen code doesn't import tracker/internal/api.
const (
	PushTagOffer      = api.PushTagOffer
	PushTagSettlement = api.PushTagSettlement
)

// DialRPCVia is DialRPCWithIdentity dialing through a caller-owned
// *quic.Transport, so many client connections can share one UDP
// socket — 20k simulated participants must not need 20k file
// descriptors. The caller keeps ownership of the transport and closes
// it after every client dialed through it is done.
func DialRPCVia(ctx context.Context, tr *quicgo.Transport, trackerAddr string, trackerSPKIHash [32]byte, priv ed25519.PrivateKey) (*RPCClient, error) {
	tlsCfg, trackerPub, err := clientTLSConfig(trackerSPKIHash, priv)
	if err != nil {
		return nil, err
	}
	udpAddr, err := net.ResolveUDPAddr("udp", trackerAddr)
	if err != nil {
		return nil, fmt.Errorf("driver: resolve %s: %w", trackerAddr, err)
	}
	conn, err := tr.Dial(ctx, udpAddr, tlsCfg, rpcQUICConfig())
	if err != nil {
		return nil, fmt.Errorf("driver: quic dial %s via shared transport: %w", trackerAddr, err)
	}
	return finishDial(ctx, conn, priv, trackerPub)
}

// AcceptPush blocks until the tracker opens a push stream on this
// connection and returns the stream positioned after its 1-byte tag
// (PushTagOffer or PushTagSettlement). The caller must Close the
// stream when done with it.
func (c *RPCClient) AcceptPush(ctx context.Context) (*quicgo.Stream, byte, error) {
	stream, err := c.conn.AcceptStream(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("driver: accept push stream: %w", err)
	}
	var tag [1]byte
	if _, err := stream.Read(tag[:]); err != nil {
		_ = stream.Close()
		return nil, 0, fmt.Errorf("driver: read push tag: %w", err)
	}
	return stream, tag[0], nil
}

// ReadPush reads one length-prefixed frame from a push stream into dst.
func (c *RPCClient) ReadPush(stream *quicgo.Stream, dst proto.Message) error {
	return readFrame(stream, dst, c.maxFrame)
}

// WritePush writes one length-prefixed frame to a push stream.
func (c *RPCClient) WritePush(stream *quicgo.Stream, m proto.Message) error {
	return writeFrame(stream, m, c.maxFrame)
}

// Heartbeat sends one HeartbeatPing on the held heartbeat stream and
// reads the pong. The heartbeat stream belongs to exactly one caller
// goroutine — same single-owner contract as Call.
func (c *RPCClient) Heartbeat(ctx context.Context, seq uint64) error {
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.hb.SetDeadline(deadline)
		defer func() { _ = c.hb.SetDeadline(time.Time{}) }()
	}
	ping := &tbproto.HeartbeatPing{Seq: seq, T: uint64(time.Now().UnixMilli())} //nolint:gosec // unix ms, positive
	if err := writeFrame(c.hb, ping, c.maxFrame); err != nil {
		return fmt.Errorf("driver: write heartbeat ping: %w", err)
	}
	var pong tbproto.HeartbeatPong
	if err := readFrame(c.hb, &pong, c.maxFrame); err != nil {
		return fmt.Errorf("driver: read heartbeat pong: %w", err)
	}
	return nil
}

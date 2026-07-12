//go:build e2e || perf

package driver

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"

	quicgo "github.com/quic-go/quic-go"
	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
	"github.com/token-bay/token-bay/tracker/internal/server"
)

// RPCClient is a persistent mTLS QUIC connection to a single tracker's
// RPC surface, generalizing the one-shot BALANCE dial in balance.go so
// scenario tests can issue ANY RpcMethod (STUN_ALLOCATE, TURN_RELAY_OPEN,
// BOOTSTRAP_PEERS, malformed-payload negatives, ...) against the live
// compose stack. One RPC per QUIC stream, mirroring the server's
// handleRPCStream contract (tracker/internal/server/rpc_stream.go); the
// dedicated heartbeat stream the server expects as the FIRST
// client-initiated stream is opened at dial time and held for the
// client's lifetime.
//
// Not safe for concurrent Calls from multiple goroutines by contract
// (scenario tests are sequential); quic-go itself would tolerate it, but
// keeping the contract narrow keeps the driver honest.
type RPCClient struct {
	conn       *quicgo.Conn
	hb         *quicgo.Stream
	trackerPub ed25519.PublicKey
	priv       ed25519.PrivateKey
	identityID [32]byte
	maxFrame   int
}

// DialRPC dials the tracker at trackerAddr (host:port, UDP — QUIC) with
// a freshly generated throwaway Ed25519 client identity, pinning the
// tracker by trackerSPKIHash exactly like BalanceOf.
func DialRPC(ctx context.Context, trackerAddr string, trackerSPKIHash [32]byte) (*RPCClient, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("driver: generate client identity: %w", err)
	}
	return DialRPCWithIdentity(ctx, trackerAddr, trackerSPKIHash, priv)
}

// DialRPCWithIdentity is DialRPC with a caller-supplied Ed25519 identity
// key, for scenarios that need a stable identity across connections (or
// need to sign envelopes under the same key the mTLS layer presents).
func DialRPCWithIdentity(ctx context.Context, trackerAddr string, trackerSPKIHash [32]byte, priv ed25519.PrivateKey) (*RPCClient, error) {
	tlsCfg, trackerPub, err := clientTLSConfig(trackerSPKIHash, priv)
	if err != nil {
		return nil, err
	}
	conn, err := quicgo.DialAddr(ctx, trackerAddr, tlsCfg, rpcQUICConfig())
	if err != nil {
		return nil, fmt.Errorf("driver: quic dial %s: %w", trackerAddr, err)
	}
	return finishDial(ctx, conn, priv, trackerPub)
}

// clientTLSConfig builds the mTLS client config that pins the tracker
// by SPKI hash. trackerPub is filled in during the handshake's
// VerifyPeerCertificate; read it only after a successful dial.
func clientTLSConfig(trackerSPKIHash [32]byte, priv ed25519.PrivateKey) (*tls.Config, *ed25519.PublicKey, error) {
	cliCert, err := server.CertFromIdentity(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("driver: build client cert: %w", err)
	}

	trackerPub := new(ed25519.PublicKey)
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cliCert},
		//nolint:gosec // G402: InsecureSkipVerify is required so quic-go
		// invokes VerifyPeerCertificate instead of doing web-PKI chain
		// validation; the SPKI pin below is the real identity check
		// (same pattern as BalanceOf / tracker/test/integration).
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return errors.New("driver: tracker presented no certificate")
			}
			parsed, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("driver: parse tracker cert: %w", err)
			}
			got := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
			if got != trackerSPKIHash {
				return fmt.Errorf("driver: tracker SPKI mismatch: got %x want %x", got, trackerSPKIHash)
			}
			pub, ok := parsed.PublicKey.(ed25519.PublicKey)
			if !ok {
				return errors.New("driver: tracker cert is not Ed25519")
			}
			*trackerPub = pub
			return nil
		},
		NextProtos:             []string{balanceALPN},
		MinVersion:             tls.VersionTLS13,
		SessionTicketsDisabled: true,
	}
	return tlsCfg, trackerPub, nil
}

// rpcQUICConfig is the QUIC config both dial paths share.
func rpcQUICConfig() *quicgo.Config {
	return &quicgo.Config{
		EnableDatagrams: false,
		Allow0RTT:       false,
	}
}

// finishDial completes RPCClient construction after the QUIC dial:
// opens the held heartbeat stream and checks the pinned tracker key.
func finishDial(ctx context.Context, conn *quicgo.Conn, priv ed25519.PrivateKey, trackerPub *ed25519.PublicKey) (*RPCClient, error) {
	id, err := clientIdentityID(priv)
	if err != nil {
		_ = conn.CloseWithError(0, "driver: identity id")
		return nil, err
	}

	// The server's serveConn blocks its first AcceptStream on a dedicated
	// heartbeat stream before entering the per-connection RPC loop — open
	// and hold one for the client's lifetime (same as BalanceOf).
	hb, err := conn.OpenStreamSync(ctx)
	if err != nil {
		_ = conn.CloseWithError(0, "driver: heartbeat open failed")
		return nil, fmt.Errorf("driver: open heartbeat stream: %w", err)
	}

	if trackerPub == nil || *trackerPub == nil {
		_ = conn.CloseWithError(0, "driver: no tracker pubkey")
		return nil, errors.New("driver: tracker pubkey not captured during handshake")
	}

	return &RPCClient{
		conn:       conn,
		hb:         hb,
		trackerPub: *trackerPub,
		priv:       priv,
		identityID: id,
		maxFrame:   maxBalanceFrameSize,
	}, nil
}

// Close tears the QUIC connection down. Idempotent enough for defer use.
func (c *RPCClient) Close() error {
	_ = c.hb.Close()
	return c.conn.CloseWithError(0, "driver: rpc client done")
}

// TrackerPub returns the tracker's Ed25519 pubkey recovered from the
// pinned certificate it presented during the handshake.
func (c *RPCClient) TrackerPub() ed25519.PublicKey { return c.trackerPub }

// PrivateKey returns the client's identity key, for scenarios that sign
// envelopes (signing.SignEnvelope) under the same identity the mTLS
// layer authenticated.
func (c *RPCClient) PrivateKey() ed25519.PrivateKey { return c.priv }

// IdentityID returns the 32-byte IdentityID the TRACKER derives for this
// client from its mTLS cert: sha256 over the DER SubjectPublicKeyInfo
// (server.SPKIToIdentityID), NOT sha256 of the raw pubkey.
func (c *RPCClient) IdentityID() [32]byte { return c.identityID }

// Call opens a fresh stream, sends one framed RpcRequest{method,payload}
// and returns the tracker's RpcResponse verbatim — including non-OK
// statuses, which are NOT converted to errors (error-path scenarios
// assert on them). The returned error covers transport problems only.
func (c *RPCClient) Call(ctx context.Context, method tbproto.RpcMethod, payload []byte) (*tbproto.RpcResponse, error) {
	return c.roundTrip(ctx, func(stream *quicgo.Stream) error {
		req := &tbproto.RpcRequest{Method: method, Payload: payload}
		if err := writeFrame(stream, req, c.maxFrame); err != nil {
			return fmt.Errorf("driver: write %s request: %w", method, err)
		}
		return nil
	})
}

// CallMsg is Call with the payload proto-marshaled from req. A nil req
// sends an empty payload.
func (c *RPCClient) CallMsg(ctx context.Context, method tbproto.RpcMethod, req proto.Message) (*tbproto.RpcResponse, error) {
	var payload []byte
	if req != nil {
		b, err := proto.Marshal(req)
		if err != nil {
			return nil, fmt.Errorf("driver: marshal %s payload: %w", method, err)
		}
		payload = b
	}
	return c.Call(ctx, method, payload)
}

// CallRawFrame writes raw bytes verbatim on a fresh stream (no framing,
// no proto marshal — the caller controls every byte, including the
// 4-byte length prefix) and reads back one framed RpcResponse. Used by
// negative scenarios (oversize length prefix, garbage frames) that must
// bypass writeFrame's own size checks.
func (c *RPCClient) CallRawFrame(ctx context.Context, raw []byte) (*tbproto.RpcResponse, error) {
	return c.roundTrip(ctx, func(stream *quicgo.Stream) error {
		if _, err := stream.Write(raw); err != nil {
			return fmt.Errorf("driver: write raw frame: %w", err)
		}
		return nil
	})
}

// roundTrip opens a stream, applies ctx's deadline to it, runs send, and
// reads one framed RpcResponse.
func (c *RPCClient) roundTrip(ctx context.Context, send func(*quicgo.Stream) error) (*tbproto.RpcResponse, error) {
	stream, err := c.conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("driver: open rpc stream: %w", err)
	}
	defer stream.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = stream.SetDeadline(deadline)
	}
	if err := send(stream); err != nil {
		return nil, err
	}
	var resp tbproto.RpcResponse
	if err := readFrame(stream, &resp, c.maxFrame); err != nil {
		return nil, fmt.Errorf("driver: read rpc response: %w", err)
	}
	return &resp, nil
}

// VerifiedBalance issues a BALANCE RPC for identityID and verifies the
// returned SignedBalanceSnapshot against the tracker pubkey pinned at
// dial time. Non-OK statuses and bad signatures are errors here — this
// is the happy-path accessor; error-path scenarios use Call directly.
func (c *RPCClient) VerifiedBalance(ctx context.Context, identityID [32]byte) (*tbproto.SignedBalanceSnapshot, error) {
	resp, err := c.CallMsg(ctx, tbproto.RpcMethod_RPC_METHOD_BALANCE, &tbproto.BalanceRequest{IdentityId: identityID[:]})
	if err != nil {
		return nil, err
	}
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		return nil, fmt.Errorf("driver: BALANCE rpc status=%s error=%v", resp.Status, resp.GetError())
	}
	var snap tbproto.SignedBalanceSnapshot
	if err := proto.Unmarshal(resp.Payload, &snap); err != nil {
		return nil, fmt.Errorf("driver: unmarshal SignedBalanceSnapshot: %w", err)
	}
	if !signing.VerifyBalanceSnapshot(c.trackerPub, &snap) {
		return nil, errors.New("driver: SignedBalanceSnapshot failed tracker signature verification")
	}
	return &snap, nil
}

// clientIdentityID computes the IdentityID the tracker will assign a
// client presenting priv's cert: sha256 over the DER-encoded
// SubjectPublicKeyInfo of the public key. Pure function; unit-tested in
// driver_test.go against server.SPKIToIdentityID.
func clientIdentityID(priv ed25519.PrivateKey) ([32]byte, error) {
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return [32]byte{}, fmt.Errorf("driver: unexpected public key type %T", priv.Public())
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return [32]byte{}, fmt.Errorf("driver: marshal SPKI: %w", err)
	}
	return sha256.Sum256(der), nil
}

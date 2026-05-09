package federation

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/shared/signing"
	"google.golang.org/protobuf/proto"
)

const protocolVersion uint32 = 1

// HandshakeResult is what the caller learns about the counterparty after
// a successful handshake.
type HandshakeResult struct {
	PeerTrackerID ids.TrackerID
	PeerPubKey    ed25519.PublicKey
	DedupeTTL     time.Duration
	GossipRateQPS uint32
}

func sendHello(ctx context.Context, conn PeerConn, priv ed25519.PrivateKey, myID ids.TrackerID) ([]byte, error) {
	nonce := make([]byte, 32)
	if _, err := crand.Read(nonce); err != nil {
		return nil, fmt.Errorf("%w: nonce: %v", ErrHandshakeFailed, err)
	}
	idBytes := myID.Bytes()
	hello := &fed.Hello{TrackerId: idBytes[:], ProtocolVersion: protocolVersion, Nonce: nonce}
	if err := fed.ValidateHello(hello); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	payload, err := proto.Marshal(hello)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal hello: %v", ErrHandshakeFailed, err)
	}
	env, err := SignEnvelope(priv, idBytes[:], fed.Kind_KIND_HELLO, payload)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	frame, err := MarshalFrame(env)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	if err := conn.Send(ctx, frame); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	return nonce, nil
}

func recvKind(ctx context.Context, conn PeerConn, want fed.Kind) (*fed.Envelope, []byte, error) {
	frame, err := conn.Recv(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: recv: %v", ErrHandshakeFailed, err)
	}
	env, err := UnmarshalFrame(frame)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	if env.Kind != want {
		return nil, nil, fmt.Errorf("%w: got kind %v want %v", ErrHandshakeFailed, env.Kind, want)
	}
	return env, env.Payload, nil
}

func sendPeerAuth(ctx context.Context, conn PeerConn, priv ed25519.PrivateKey, myID ids.TrackerID, theirNonce []byte) error {
	auth := &fed.PeerAuth{NonceSig: ed25519.Sign(priv, theirNonce)}
	if err := fed.ValidatePeerAuth(auth); err != nil {
		return fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	payload, err := proto.Marshal(auth)
	if err != nil {
		return fmt.Errorf("%w: marshal peer_auth: %v", ErrHandshakeFailed, err)
	}
	idBytes := myID.Bytes()
	env, err := SignEnvelope(priv, idBytes[:], fed.Kind_KIND_PEER_AUTH, payload)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	frame, err := MarshalFrame(env)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	if err := conn.Send(ctx, frame); err != nil {
		return fmt.Errorf("%w: send peer_auth: %v", ErrHandshakeFailed, err)
	}
	return nil
}

// validatePeerHello unmarshals + validates a peer's Hello envelope, looks
// up the expected pubkey in the allowlist, verifies envelope sig, and
// returns the peer's TrackerID + pubkey + nonce.
func validatePeerHello(env *fed.Envelope, expected map[ids.TrackerID]ed25519.PublicKey) (ids.TrackerID, ed25519.PublicKey, []byte, error) {
	var hello fed.Hello
	if err := proto.Unmarshal(env.Payload, &hello); err != nil {
		return ids.TrackerID{}, nil, nil, fmt.Errorf("%w: hello unmarshal: %v", ErrHandshakeFailed, err)
	}
	if err := fed.ValidateHello(&hello); err != nil {
		return ids.TrackerID{}, nil, nil, fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	if hello.ProtocolVersion != protocolVersion {
		return ids.TrackerID{}, nil, nil, fmt.Errorf("%w: protocol_version %d != %d", ErrHandshakeFailed, hello.ProtocolVersion, protocolVersion)
	}
	var tid ids.TrackerID
	copy(tid[:], hello.TrackerId)
	pub, ok := expected[tid]
	if !ok {
		return ids.TrackerID{}, nil, nil, fmt.Errorf("%w: tracker %x not in allowlist", ErrHandshakeFailed, hello.TrackerId)
	}
	if !signing.Verify(pub, env.Payload, env.SenderSig) {
		return ids.TrackerID{}, nil, nil, fmt.Errorf("%w: hello envelope sig", ErrHandshakeFailed)
	}
	if want := sha256.Sum256(pub); want != tid {
		return ids.TrackerID{}, nil, nil, fmt.Errorf("%w: hello tracker_id != hash(pubkey)", ErrHandshakeFailed)
	}
	return tid, pub, hello.Nonce, nil
}

func validatePeerAuth(env *fed.Envelope, pub ed25519.PublicKey, myNonce []byte) error {
	var auth fed.PeerAuth
	if err := proto.Unmarshal(env.Payload, &auth); err != nil {
		return fmt.Errorf("%w: peer_auth unmarshal: %v", ErrHandshakeFailed, err)
	}
	if err := fed.ValidatePeerAuth(&auth); err != nil {
		return fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	if !signing.Verify(pub, env.Payload, env.SenderSig) {
		return fmt.Errorf("%w: peer_auth envelope sig", ErrHandshakeFailed)
	}
	if !signing.Verify(pub, myNonce, auth.NonceSig) {
		return fmt.Errorf("%w: peer_auth nonce sig", ErrHandshakeFailed)
	}
	return nil
}

func sendAccept(ctx context.Context, conn PeerConn, priv ed25519.PrivateKey, myID ids.TrackerID, ttl time.Duration, qps uint32) error {
	ttlS := ttl / time.Second
	acc := &fed.PeeringAccept{DedupeTtlS: uint32(ttlS), GossipRateQps: qps} //nolint:gosec // ttl is caller-controlled; overflow not reachable in practice
	payload, err := proto.Marshal(acc)
	if err != nil {
		return fmt.Errorf("%w: marshal accept: %v", ErrHandshakeFailed, err)
	}
	idBytes := myID.Bytes()
	env, err := SignEnvelope(priv, idBytes[:], fed.Kind_KIND_PEERING_ACCEPT, payload)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	frame, err := MarshalFrame(env)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	if err := conn.Send(ctx, frame); err != nil {
		return fmt.Errorf("%w: send accept: %v", ErrHandshakeFailed, err)
	}
	return nil
}

// RunHandshakeDialer is called by the side that initiated the connection.
// Sequence: send Hello → recv peer Hello → send PeerAuth → recv PeerAuth
// → recv PeeringAccept|Reject.
func RunHandshakeDialer(ctx context.Context, conn PeerConn, myID ids.TrackerID, priv ed25519.PrivateKey, peerID ids.TrackerID, peerPub ed25519.PublicKey, timeout time.Duration) (HandshakeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	myNonce, err := sendHello(ctx, conn, priv, myID)
	if err != nil {
		return HandshakeResult{}, err
	}
	helloEnv, _, err := recvKind(ctx, conn, fed.Kind_KIND_HELLO)
	if err != nil {
		return HandshakeResult{}, err
	}
	gotID, gotPub, theirNonce, err := validatePeerHello(helloEnv, map[ids.TrackerID]ed25519.PublicKey{peerID: peerPub})
	if err != nil {
		return HandshakeResult{}, err
	}
	if err := sendPeerAuth(ctx, conn, priv, myID, theirNonce); err != nil {
		return HandshakeResult{}, err
	}
	authEnv, _, err := recvKind(ctx, conn, fed.Kind_KIND_PEER_AUTH)
	if err != nil {
		return HandshakeResult{}, err
	}
	if err := validatePeerAuth(authEnv, gotPub, myNonce); err != nil {
		return HandshakeResult{}, err
	}
	accEnv, accPayload, err := recvKind(ctx, conn, fed.Kind_KIND_PEERING_ACCEPT)
	if err != nil {
		return HandshakeResult{}, err
	}
	if !signing.Verify(gotPub, accEnv.Payload, accEnv.SenderSig) {
		return HandshakeResult{}, fmt.Errorf("%w: accept envelope sig", ErrHandshakeFailed)
	}
	var acc fed.PeeringAccept
	if err := proto.Unmarshal(accPayload, &acc); err != nil {
		return HandshakeResult{}, fmt.Errorf("%w: unmarshal accept: %v", ErrHandshakeFailed, err)
	}
	return HandshakeResult{
		PeerTrackerID: gotID,
		PeerPubKey:    gotPub,
		DedupeTTL:     time.Duration(acc.DedupeTtlS) * time.Second,
		GossipRateQPS: acc.GossipRateQps,
	}, nil
}

// RunHandshakeListener is called by the side that accepted the connection.
// Sequence: recv peer Hello → send Hello → recv PeerAuth → send PeerAuth
// → send PeeringAccept.
func RunHandshakeListener(ctx context.Context, conn PeerConn, myID ids.TrackerID, priv ed25519.PrivateKey, expected map[ids.TrackerID]ed25519.PublicKey, timeout time.Duration) (HandshakeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	helloEnv, _, err := recvKind(ctx, conn, fed.Kind_KIND_HELLO)
	if err != nil {
		return HandshakeResult{}, err
	}
	gotID, gotPub, theirNonce, err := validatePeerHello(helloEnv, expected)
	if err != nil {
		return HandshakeResult{}, err
	}
	myNonce, err := sendHello(ctx, conn, priv, myID)
	if err != nil {
		return HandshakeResult{}, err
	}
	authEnv, _, err := recvKind(ctx, conn, fed.Kind_KIND_PEER_AUTH)
	if err != nil {
		return HandshakeResult{}, err
	}
	if err := validatePeerAuth(authEnv, gotPub, myNonce); err != nil {
		return HandshakeResult{}, err
	}
	if err := sendPeerAuth(ctx, conn, priv, myID, theirNonce); err != nil {
		return HandshakeResult{}, err
	}
	if err := sendAccept(ctx, conn, priv, myID, time.Hour, 100); err != nil {
		return HandshakeResult{}, err
	}
	return HandshakeResult{PeerTrackerID: gotID, PeerPubKey: gotPub, DedupeTTL: time.Hour, GossipRateQPS: 100}, nil
}

var _ = errors.New // keep import

// Package main implements fedactor: a controllable, deliberately Byzantine
// federation-neighbor actor for the tracker e2e suite. It impersonates a
// neighbor tracker — dialing a real tracker's federation listener over QUIC
// (ALPN tokenbay-fed/1), completing the dialer handshake, and then emitting
// arbitrary signed federation messages on command (well-formed and
// conflicting ROOT_ATTESTATIONs for equivocation, REVOCATIONs, etc.).
//
// It lives under tracker/test/e2e so it can import tracker/internal/federation
// and reuse the exact wire path a real peer would.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/federation"
	"google.golang.org/protobuf/proto"
)

// handshakeTimeout bounds the dialer handshake and each subsequent frame op.
const handshakeTimeout = 5 * time.Second

// recvRecord is a decoded inbound frame, kept for the /received endpoint so
// the driver can assert on kinds/senders (e.g. EQUIVOCATION_EVIDENCE emitted
// by a victim after it detects our conflicting roots).
type recvRecord struct {
	Kind     string `json:"kind"`
	SenderID string `json:"sender_id"` // hex
	At       string `json:"at"`        // RFC3339
}

// Actor holds the fedactor signing state and the single live peer connection.
// The same PeerConn is reused for every frame after the handshake — one
// persistent bidi stream, never a new one.
type Actor struct {
	priv  ed25519.PrivateKey
	pub   ed25519.PublicKey
	fedID ids.TrackerID

	mu       sync.Mutex
	conn     federation.PeerConn
	received []recvRecord
}

// NewActor builds an Actor from a raw 64-byte ed25519 private key. fedID is
// derived as sha256(pubkey) — the federation tracker_id convention, which
// must equal Envelope.sender_id on everything we emit.
func NewActor(priv ed25519.PrivateKey) (*Actor, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("fedactor: priv key len %d, want %d", len(priv), ed25519.PrivateKeySize)
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("fedactor: private key does not yield an ed25519 public key")
	}
	return &Actor{
		priv:  priv,
		pub:   pub,
		fedID: ids.TrackerID(sha256.Sum256(pub)),
	}, nil
}

// FedID returns the derived tracker_id (sha256 of the pubkey).
func (a *Actor) FedID() ids.TrackerID { return a.fedID }

// handshake dials targetAddr (QUIC + mTLS SPKI pin computed internally by
// Dial), runs the dialer handshake, stores the live PeerConn, and starts a
// background goroutine draining inbound frames into a.received.
func (a *Actor) handshake(ctx context.Context, targetAddr, targetPubHex string) error {
	pubBytes, err := hex.DecodeString(targetPubHex)
	if err != nil {
		return fmt.Errorf("fedactor: decode target pubkey hex: %w", err)
	}
	if len(pubBytes) != ed25519.PublicKeySize {
		return fmt.Errorf("fedactor: target pubkey len %d, want %d", len(pubBytes), ed25519.PublicKeySize)
	}
	targetPub := ed25519.PublicKey(pubBytes)
	targetID := ids.TrackerID(sha256.Sum256(targetPub))

	cert, err := federation.CertFromIdentity(a.priv)
	if err != nil {
		return fmt.Errorf("fedactor: cert from identity: %w", err)
	}
	tr, err := federation.NewQUICTransport(federation.QUICConfig{Cert: cert})
	if err != nil {
		return fmt.Errorf("fedactor: new transport: %w", err)
	}
	conn, err := tr.Dial(ctx, targetAddr, targetPub)
	if err != nil {
		return fmt.Errorf("fedactor: dial %s: %w", targetAddr, err)
	}
	if _, err := federation.RunHandshakeDialer(ctx, conn, a.fedID, a.priv, targetID, targetPub, handshakeTimeout); err != nil {
		_ = conn.Close()
		return fmt.Errorf("fedactor: handshake: %w", err)
	}

	a.mu.Lock()
	a.conn = conn
	a.mu.Unlock()

	go a.drain(conn)
	return nil
}

// drain reads frames off the persistent stream until it errors (peer close),
// decoding each far enough to record kind + sender for /received.
func (a *Actor) drain(conn federation.PeerConn) {
	for {
		frame, err := conn.Recv(context.Background())
		if err != nil {
			return
		}
		env, err := federation.UnmarshalFrame(frame)
		if err != nil {
			continue
		}
		a.mu.Lock()
		a.received = append(a.received, recvRecord{
			Kind:     env.Kind.String(),
			SenderID: hex.EncodeToString(env.SenderId),
			At:       time.Now().UTC().Format(time.RFC3339),
		})
		a.mu.Unlock()
	}
}

// receivedRecords returns a copy of the drained inbound records.
func (a *Actor) receivedRecords() []recvRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]recvRecord, len(a.received))
	copy(out, a.received)
	return out
}

// send transmits a pre-built frame over the persistent PeerConn.
func (a *Actor) send(ctx context.Context, frame []byte) error {
	a.mu.Lock()
	conn := a.conn
	a.mu.Unlock()
	if conn == nil {
		return errors.New("fedactor: not connected — call /handshake first")
	}
	sendCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	return conn.Send(sendCtx, frame)
}

// --- Frame builders (pure; testable without a live connection) ---

// buildEnvelopeFrame marshals an inner proto, signs the envelope under the
// fedactor key with sender_id = fedID, and returns the MarshalFrame bytes.
func (a *Actor) buildEnvelopeFrame(kind fed.Kind, inner proto.Message) ([]byte, error) {
	payload, err := proto.Marshal(inner)
	if err != nil {
		return nil, fmt.Errorf("fedactor: marshal %s payload: %w", kind, err)
	}
	idBytes := a.fedID.Bytes()
	env, err := federation.SignEnvelope(a.priv, idBytes[:], kind, payload)
	if err != nil {
		return nil, fmt.Errorf("fedactor: sign %s envelope: %w", kind, err)
	}
	frame, err := federation.MarshalFrame(env)
	if err != nil {
		return nil, fmt.Errorf("fedactor: marshal %s frame: %w", kind, err)
	}
	return frame, nil
}

// buildRootAttestationFrame builds a signed KIND_ROOT_ATTESTATION for our own
// tracker_id at hour with the given 32-byte merkle root. tracker_sig is
// arbitrary (never cryptographically verified on receipt) but must be 64
// bytes to pass shape validation; we sign the root so it is deterministic.
func (a *Actor) buildRootAttestationFrame(hour uint64, root []byte) ([]byte, error) {
	if len(root) != fed.RootLen {
		return nil, fmt.Errorf("fedactor: merkle_root len %d, want %d", len(root), fed.RootLen)
	}
	idBytes := a.fedID.Bytes()
	ra := &fed.RootAttestation{
		TrackerId:  idBytes[:],
		Hour:       hour,
		MerkleRoot: append([]byte(nil), root...),
		TrackerSig: ed25519.Sign(a.priv, root),
	}
	if err := fed.ValidateRootAttestation(ra); err != nil {
		return nil, fmt.Errorf("fedactor: invalid root_attestation: %w", err)
	}
	return a.buildEnvelopeFrame(fed.Kind_KIND_ROOT_ATTESTATION, ra)
}

// buildEquivocationFrame builds a signed KIND_EQUIVOCATION_EVIDENCE naming our
// own tracker_id as the offender, with two distinct 32-byte roots.
func (a *Actor) buildEquivocationFrame(hour uint64, rootA, rootB []byte) ([]byte, error) {
	if len(rootA) != fed.RootLen || len(rootB) != fed.RootLen {
		return nil, fmt.Errorf("fedactor: evidence roots must be %d bytes", fed.RootLen)
	}
	idBytes := a.fedID.Bytes()
	evi := &fed.EquivocationEvidence{
		TrackerId: idBytes[:],
		Hour:      hour,
		RootA:     append([]byte(nil), rootA...),
		SigA:      ed25519.Sign(a.priv, rootA),
		RootB:     append([]byte(nil), rootB...),
		SigB:      ed25519.Sign(a.priv, rootB),
	}
	if err := fed.ValidateEquivocationEvidence(evi); err != nil {
		return nil, fmt.Errorf("fedactor: invalid equivocation_evidence: %w", err)
	}
	return a.buildEnvelopeFrame(fed.Kind_KIND_EQUIVOCATION_EVIDENCE, evi)
}

// buildRevocationFrame builds a signed KIND_REVOCATION. Unlike the attestation
// tracker_sig, Revocation.tracker_sig IS verified by the victim, so it is
// signed over CanonicalRevocationPreSig under the fedactor key.
func (a *Actor) buildRevocationFrame(identity []byte, reason int) ([]byte, error) {
	if len(identity) != fed.TrackerIDLen {
		return nil, fmt.Errorf("fedactor: identity_id len %d, want %d", len(identity), fed.TrackerIDLen)
	}
	r := fed.RevocationReason(reason) //nolint:gosec // caller-supplied; range-checked by ValidateRevocation below
	idBytes := a.fedID.Bytes()
	rev := &fed.Revocation{
		TrackerId:  idBytes[:],
		IdentityId: append([]byte(nil), identity...),
		Reason:     r,
		RevokedAt:  uint64(time.Now().Unix()), //nolint:gosec // G115 — Unix() ≥ 0 post-epoch
	}
	canonical, err := fed.CanonicalRevocationPreSig(rev)
	if err != nil {
		return nil, fmt.Errorf("fedactor: canonical revocation: %w", err)
	}
	rev.TrackerSig = ed25519.Sign(a.priv, canonical)
	if err := fed.ValidateRevocation(rev); err != nil {
		return nil, fmt.Errorf("fedactor: invalid revocation: %w", err)
	}
	return a.buildEnvelopeFrame(fed.Kind_KIND_REVOCATION, rev)
}

// --- Send helpers (build + transmit over the live conn) ---

// sendRootAttestation signs and sends one ROOT_ATTESTATION. Sending two with
// the same hour and different roots triggers the victim's equivocation path.
func (a *Actor) sendRootAttestation(ctx context.Context, hour uint64, rootHex string) error {
	root, err := hex.DecodeString(rootHex)
	if err != nil {
		return fmt.Errorf("fedactor: decode root hex: %w", err)
	}
	frame, err := a.buildRootAttestationFrame(hour, root)
	if err != nil {
		return err
	}
	return a.send(ctx, frame)
}

// sendEquivocationEvidence signs and sends one EQUIVOCATION_EVIDENCE.
func (a *Actor) sendEquivocationEvidence(ctx context.Context, hour uint64, rootAHex, rootBHex string) error {
	rootA, err := hex.DecodeString(rootAHex)
	if err != nil {
		return fmt.Errorf("fedactor: decode root_a hex: %w", err)
	}
	rootB, err := hex.DecodeString(rootBHex)
	if err != nil {
		return fmt.Errorf("fedactor: decode root_b hex: %w", err)
	}
	frame, err := a.buildEquivocationFrame(hour, rootA, rootB)
	if err != nil {
		return err
	}
	return a.send(ctx, frame)
}

// sendRevocation signs and sends one REVOCATION for identityHex with reason.
func (a *Actor) sendRevocation(ctx context.Context, identityHex string, reason int) error {
	identity, err := hex.DecodeString(identityHex)
	if err != nil {
		return fmt.Errorf("fedactor: decode identity hex: %w", err)
	}
	frame, err := a.buildRevocationFrame(identity, reason)
	if err != nil {
		return err
	}
	return a.send(ctx, frame)
}

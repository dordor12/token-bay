package federation

import (
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/shared/signing"
	"google.golang.org/protobuf/proto"
)

// SignEnvelope wraps payload in an Envelope signed by priv.
//
// payload MUST already be the Marshal() bytes of the inner proto. The
// caller is responsible for shape-validating the inner message before
// signing (see shared/federation.Validate*).
func SignEnvelope(priv ed25519.PrivateKey, senderID []byte, kind fed.Kind, payload []byte) (*fed.Envelope, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, errors.New("federation: SignEnvelope requires Ed25519 private key")
	}
	if len(senderID) != fed.TrackerIDLen {
		return nil, fmt.Errorf("federation: senderID must be %d bytes", fed.TrackerIDLen)
	}
	env := &fed.Envelope{
		SenderId:  append([]byte(nil), senderID...),
		Kind:      kind,
		Payload:   append([]byte(nil), payload...),
		SenderSig: ed25519.Sign(priv, payload),
	}
	if err := fed.ValidateEnvelope(env); err != nil {
		return nil, err
	}
	return env, nil
}

// VerifyEnvelope returns nil iff e is well-shaped and e.SenderSig is
// valid Ed25519 over e.Payload under pub.
func VerifyEnvelope(pub ed25519.PublicKey, e *fed.Envelope) error {
	if err := fed.ValidateEnvelope(e); err != nil {
		return err
	}
	if !signing.Verify(pub, e.Payload, e.SenderSig) {
		return ErrSigInvalid
	}
	return nil
}

// MessageID is sha256(payload). It is computed over the inner payload
// only — NOT the envelope — so the same inner message gossiped via two
// paths produces the same id even though sender_sig differs.
func MessageID(e *fed.Envelope) [32]byte {
	return sha256.Sum256(e.Payload)
}

// MarshalFrame serializes an Envelope. Receivers feed the bytes back
// through UnmarshalFrame.
func MarshalFrame(e *fed.Envelope) ([]byte, error) {
	if err := fed.ValidateEnvelope(e); err != nil {
		return nil, err
	}
	b, err := proto.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("federation: marshal frame: %w", err)
	}
	return b, nil
}

// UnmarshalFrame parses a frame into an Envelope and validates its shape.
// Returns ErrFrameTooLarge if the frame is over 1 MiB.
func UnmarshalFrame(frame []byte) (*fed.Envelope, error) {
	if len(frame) > MaxFrameBytes {
		return nil, ErrFrameTooLarge
	}
	var e fed.Envelope
	if err := proto.Unmarshal(frame, &e); err != nil {
		return nil, fmt.Errorf("federation: unmarshal frame: %w", err)
	}
	if err := fed.ValidateEnvelope(&e); err != nil {
		return nil, err
	}
	return &e, nil
}

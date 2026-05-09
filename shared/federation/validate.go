package federation

import (
	"bytes"
	"errors"
	"fmt"
)

const (
	trackerIDLen = 32
	rootLen      = 32
	sigLen       = 64
	nonceLen     = 32
)

// ValidateEnvelope enforces shape invariants on an Envelope. Receivers
// MUST call this before verifying sender_sig or dispatching by Kind.
// Senders MUST call it before signing.
func ValidateEnvelope(e *Envelope) error {
	if e == nil {
		return errors.New("federation: nil Envelope")
	}
	if len(e.SenderId) != trackerIDLen {
		return fmt.Errorf("federation: sender_id len %d != %d", len(e.SenderId), trackerIDLen)
	}
	if e.Kind <= Kind_KIND_UNSPECIFIED || e.Kind > Kind_KIND_PONG {
		return fmt.Errorf("federation: kind %d out of range", int32(e.Kind))
	}
	if len(e.Payload) == 0 {
		return errors.New("federation: payload empty")
	}
	if len(e.SenderSig) != sigLen {
		return fmt.Errorf("federation: sender_sig len %d != %d", len(e.SenderSig), sigLen)
	}
	return nil
}

func ValidateHello(h *Hello) error {
	if h == nil {
		return errors.New("federation: nil Hello")
	}
	if len(h.TrackerId) != trackerIDLen {
		return fmt.Errorf("federation: hello.tracker_id len %d != %d", len(h.TrackerId), trackerIDLen)
	}
	if len(h.Nonce) != nonceLen {
		return fmt.Errorf("federation: hello.nonce len %d != %d", len(h.Nonce), nonceLen)
	}
	return nil
}

func ValidatePeerAuth(p *PeerAuth) error {
	if p == nil {
		return errors.New("federation: nil PeerAuth")
	}
	if len(p.NonceSig) != sigLen {
		return fmt.Errorf("federation: peerauth.nonce_sig len %d != %d", len(p.NonceSig), sigLen)
	}
	return nil
}

func ValidateRootAttestation(m *RootAttestation) error {
	if m == nil {
		return errors.New("federation: nil RootAttestation")
	}
	if len(m.TrackerId) != trackerIDLen || allZero(m.TrackerId) {
		return fmt.Errorf("federation: root_attestation.tracker_id invalid")
	}
	if m.Hour == 0 {
		return errors.New("federation: root_attestation.hour must be > 0")
	}
	if len(m.MerkleRoot) != rootLen {
		return fmt.Errorf("federation: root_attestation.merkle_root len %d != %d", len(m.MerkleRoot), rootLen)
	}
	if len(m.TrackerSig) != sigLen {
		return fmt.Errorf("federation: root_attestation.tracker_sig len %d != %d", len(m.TrackerSig), sigLen)
	}
	return nil
}

func ValidateEquivocationEvidence(m *EquivocationEvidence) error {
	if m == nil {
		return errors.New("federation: nil EquivocationEvidence")
	}
	if len(m.TrackerId) != trackerIDLen {
		return fmt.Errorf("federation: evidence.tracker_id len %d != %d", len(m.TrackerId), trackerIDLen)
	}
	if len(m.RootA) != rootLen || len(m.RootB) != rootLen {
		return errors.New("federation: evidence.root_a/b must be 32 bytes")
	}
	if len(m.SigA) != sigLen || len(m.SigB) != sigLen {
		return errors.New("federation: evidence.sig_a/b must be 64 bytes")
	}
	if bytes.Equal(m.RootA, m.RootB) {
		return errors.New("federation: evidence.root_a == root_b (no equivocation)")
	}
	return nil
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

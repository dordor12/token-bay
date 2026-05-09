package federation

import (
	"bytes"
	"errors"
	"fmt"
)

const (
	TrackerIDLen = 32
	RootLen      = 32
	SigLen       = 64
	NonceLen     = 32
)

// ValidateEnvelope enforces shape invariants on an Envelope. Receivers
// MUST call this before verifying sender_sig or dispatching by Kind.
// Senders MUST call it before signing.
func ValidateEnvelope(e *Envelope) error {
	if e == nil {
		return errors.New("federation: nil Envelope")
	}
	if len(e.SenderId) != TrackerIDLen {
		return fmt.Errorf("federation: sender_id len %d != %d", len(e.SenderId), TrackerIDLen)
	}
	if e.Kind <= Kind_KIND_UNSPECIFIED || e.Kind > Kind_KIND_PONG {
		return fmt.Errorf("federation: kind %d out of range", int32(e.Kind))
	}
	if len(e.Payload) == 0 {
		return errors.New("federation: payload empty")
	}
	if len(e.SenderSig) != SigLen {
		return fmt.Errorf("federation: sender_sig len %d != %d", len(e.SenderSig), SigLen)
	}
	return nil
}

func ValidateHello(h *Hello) error {
	if h == nil {
		return errors.New("federation: nil Hello")
	}
	if len(h.TrackerId) != TrackerIDLen {
		return fmt.Errorf("federation: hello.tracker_id len %d != %d", len(h.TrackerId), TrackerIDLen)
	}
	if len(h.Nonce) != NonceLen {
		return fmt.Errorf("federation: hello.nonce len %d != %d", len(h.Nonce), NonceLen)
	}
	return nil
}

func ValidatePeerAuth(p *PeerAuth) error {
	if p == nil {
		return errors.New("federation: nil PeerAuth")
	}
	if len(p.NonceSig) != SigLen {
		return fmt.Errorf("federation: peerauth.nonce_sig len %d != %d", len(p.NonceSig), SigLen)
	}
	return nil
}

func ValidateRootAttestation(m *RootAttestation) error {
	if m == nil {
		return errors.New("federation: nil RootAttestation")
	}
	if len(m.TrackerId) != TrackerIDLen {
		return fmt.Errorf("federation: root_attestation.tracker_id len %d != %d", len(m.TrackerId), TrackerIDLen)
	}
	if allZero(m.TrackerId) {
		return errors.New("federation: root_attestation.tracker_id is all zero")
	}
	if m.Hour == 0 {
		return errors.New("federation: root_attestation.hour must be > 0")
	}
	if len(m.MerkleRoot) != RootLen {
		return fmt.Errorf("federation: root_attestation.merkle_root len %d != %d", len(m.MerkleRoot), RootLen)
	}
	if len(m.TrackerSig) != SigLen {
		return fmt.Errorf("federation: root_attestation.tracker_sig len %d != %d", len(m.TrackerSig), SigLen)
	}
	return nil
}

func ValidateEquivocationEvidence(m *EquivocationEvidence) error {
	if m == nil {
		return errors.New("federation: nil EquivocationEvidence")
	}
	if len(m.TrackerId) != TrackerIDLen {
		return fmt.Errorf("federation: evidence.tracker_id len %d != %d", len(m.TrackerId), TrackerIDLen)
	}
	if len(m.RootA) != RootLen {
		return fmt.Errorf("federation: evidence.root_a len %d != %d", len(m.RootA), RootLen)
	}
	if len(m.RootB) != RootLen {
		return fmt.Errorf("federation: evidence.root_b len %d != %d", len(m.RootB), RootLen)
	}
	if len(m.SigA) != SigLen {
		return fmt.Errorf("federation: evidence.sig_a len %d != %d", len(m.SigA), SigLen)
	}
	if len(m.SigB) != SigLen {
		return fmt.Errorf("federation: evidence.sig_b len %d != %d", len(m.SigB), SigLen)
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

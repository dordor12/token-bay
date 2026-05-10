package federation

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"
)

const (
	TrackerIDLen  = 32
	RootLen       = 32
	SigLen        = 64
	NonceLen      = 32
	Ed25519PubLen = 32
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
	if e.Kind <= Kind_KIND_UNSPECIFIED || e.Kind > Kind_KIND_PEER_EXCHANGE {
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

// ValidateTransferProofRequest enforces shape invariants on a
// TransferProofRequest. Receivers MUST call this before verifying any
// signature or dispatching it to LedgerHooks.AppendTransferOut.
func ValidateTransferProofRequest(m *TransferProofRequest) error {
	if m == nil {
		return errors.New("federation: nil TransferProofRequest")
	}
	if len(m.SourceTrackerId) != TrackerIDLen {
		return fmt.Errorf("federation: transfer_request.source_tracker_id len %d != %d", len(m.SourceTrackerId), TrackerIDLen)
	}
	if len(m.DestTrackerId) != TrackerIDLen {
		return fmt.Errorf("federation: transfer_request.dest_tracker_id len %d != %d", len(m.DestTrackerId), TrackerIDLen)
	}
	if len(m.IdentityId) != TrackerIDLen {
		return fmt.Errorf("federation: transfer_request.identity_id len %d != %d", len(m.IdentityId), TrackerIDLen)
	}
	if m.Amount == 0 {
		return errors.New("federation: transfer_request.amount must be > 0")
	}
	if len(m.Nonce) != NonceLen {
		return fmt.Errorf("federation: transfer_request.nonce len %d != %d", len(m.Nonce), NonceLen)
	}
	if len(m.ConsumerSig) != SigLen {
		return fmt.Errorf("federation: transfer_request.consumer_sig len %d != %d", len(m.ConsumerSig), SigLen)
	}
	if len(m.ConsumerPub) != Ed25519PubLen {
		return fmt.Errorf("federation: transfer_request.consumer_pub len %d != %d", len(m.ConsumerPub), Ed25519PubLen)
	}
	if bytes.Equal(m.SourceTrackerId, m.DestTrackerId) {
		return errors.New("federation: transfer_request.source_tracker_id == dest_tracker_id")
	}
	if allZero(m.SourceTrackerId) {
		return errors.New("federation: transfer_request.source_tracker_id is all zero")
	}
	if allZero(m.DestTrackerId) {
		return errors.New("federation: transfer_request.dest_tracker_id is all zero")
	}
	if allZero(m.IdentityId) {
		return errors.New("federation: transfer_request.identity_id is all zero")
	}
	if allZero(m.Nonce) {
		return errors.New("federation: transfer_request.nonce is all zero")
	}
	if m.Timestamp == 0 {
		return errors.New("federation: transfer_request.timestamp must be > 0")
	}
	return nil
}

// ValidateTransferProof enforces shape invariants on a TransferProof.
func ValidateTransferProof(m *TransferProof) error {
	if m == nil {
		return errors.New("federation: nil TransferProof")
	}
	if len(m.SourceTrackerId) != TrackerIDLen {
		return fmt.Errorf("federation: transfer_proof.source_tracker_id len %d != %d", len(m.SourceTrackerId), TrackerIDLen)
	}
	if len(m.DestTrackerId) != TrackerIDLen {
		return fmt.Errorf("federation: transfer_proof.dest_tracker_id len %d != %d", len(m.DestTrackerId), TrackerIDLen)
	}
	if len(m.IdentityId) != TrackerIDLen {
		return fmt.Errorf("federation: transfer_proof.identity_id len %d != %d", len(m.IdentityId), TrackerIDLen)
	}
	if m.Amount == 0 {
		return errors.New("federation: transfer_proof.amount must be > 0")
	}
	if len(m.Nonce) != NonceLen {
		return fmt.Errorf("federation: transfer_proof.nonce len %d != %d", len(m.Nonce), NonceLen)
	}
	if len(m.SourceChainTipHash) != RootLen {
		return fmt.Errorf("federation: transfer_proof.source_chain_tip_hash len %d != %d", len(m.SourceChainTipHash), RootLen)
	}
	if len(m.SourceTrackerSig) != SigLen {
		return fmt.Errorf("federation: transfer_proof.source_tracker_sig len %d != %d", len(m.SourceTrackerSig), SigLen)
	}
	if bytes.Equal(m.SourceTrackerId, m.DestTrackerId) {
		return errors.New("federation: transfer_proof.source_tracker_id == dest_tracker_id")
	}
	if m.Timestamp == 0 {
		return errors.New("federation: transfer_proof.timestamp must be > 0")
	}
	return nil
}

// ValidateRevocation enforces shape invariants on a Revocation. Callers
// MUST verify the issuer's tracker_sig separately (validation only checks
// shape, not signatures).
func ValidateRevocation(m *Revocation) error {
	if m == nil {
		return errors.New("federation: nil Revocation")
	}
	if len(m.TrackerId) != TrackerIDLen {
		return fmt.Errorf("federation: revocation.tracker_id len %d != %d", len(m.TrackerId), TrackerIDLen)
	}
	if allZero(m.TrackerId) {
		return errors.New("federation: revocation.tracker_id is all zero")
	}
	if len(m.IdentityId) != TrackerIDLen {
		return fmt.Errorf("federation: revocation.identity_id len %d != %d", len(m.IdentityId), TrackerIDLen)
	}
	if allZero(m.IdentityId) {
		return errors.New("federation: revocation.identity_id is all zero")
	}
	if len(m.TrackerSig) != SigLen {
		return fmt.Errorf("federation: revocation.tracker_sig len %d != %d", len(m.TrackerSig), SigLen)
	}
	if m.RevokedAt == 0 {
		return errors.New("federation: revocation.revoked_at must be > 0")
	}
	if m.Reason <= RevocationReason_REVOCATION_REASON_UNSPECIFIED || m.Reason > RevocationReason_REVOCATION_REASON_EXPIRED {
		return fmt.Errorf("federation: revocation.reason %d out of range", int32(m.Reason))
	}
	return nil
}

// ValidateTransferApplied enforces shape invariants on a TransferApplied.
func ValidateTransferApplied(m *TransferApplied) error {
	if m == nil {
		return errors.New("federation: nil TransferApplied")
	}
	if len(m.SourceTrackerId) != TrackerIDLen {
		return fmt.Errorf("federation: transfer_applied.source_tracker_id len %d != %d", len(m.SourceTrackerId), TrackerIDLen)
	}
	if len(m.DestTrackerId) != TrackerIDLen {
		return fmt.Errorf("federation: transfer_applied.dest_tracker_id len %d != %d", len(m.DestTrackerId), TrackerIDLen)
	}
	if len(m.Nonce) != NonceLen {
		return fmt.Errorf("federation: transfer_applied.nonce len %d != %d", len(m.Nonce), NonceLen)
	}
	if len(m.DestTrackerSig) != SigLen {
		return fmt.Errorf("federation: transfer_applied.dest_tracker_sig len %d != %d", len(m.DestTrackerSig), SigLen)
	}
	if bytes.Equal(m.SourceTrackerId, m.DestTrackerId) {
		return errors.New("federation: transfer_applied.source_tracker_id == dest_tracker_id")
	}
	if m.Timestamp == 0 {
		return errors.New("federation: transfer_applied.timestamp must be > 0")
	}
	return nil
}

// MaxPeerExchangeEntries caps the number of KnownPeer entries inside a
// single PeerExchange message at the validator boundary. The emitter
// caps itself lower (defaultPeerExchangeEmitCap = 256 in
// tracker/internal/federation); the validator allows headroom for
// future growth without DoS.
const MaxPeerExchangeEntries = 1024

// MaxKnownPeerAddrLen and MaxKnownPeerRegionHintLen are byte-length
// caps on the corresponding string fields.
const (
	MaxKnownPeerAddrLen       = 256
	MaxKnownPeerRegionHintLen = 64
)

// ValidatePeerExchange enforces shape invariants on a PeerExchange.
// Entries are advisory hints; this only checks shape, never trust.
func ValidatePeerExchange(m *PeerExchange) error {
	if m == nil {
		return errors.New("federation: nil PeerExchange")
	}
	if len(m.Peers) > MaxPeerExchangeEntries {
		return fmt.Errorf("federation: peer_exchange.peers len %d > %d", len(m.Peers), MaxPeerExchangeEntries)
	}
	for i, p := range m.Peers {
		if p == nil {
			return fmt.Errorf("federation: peer_exchange.peers[%d] is nil", i)
		}
		if len(p.TrackerId) != TrackerIDLen {
			return fmt.Errorf("federation: peer_exchange.peers[%d].tracker_id len %d != %d", i, len(p.TrackerId), TrackerIDLen)
		}
		if allZero(p.TrackerId) {
			return fmt.Errorf("federation: peer_exchange.peers[%d].tracker_id is all zero", i)
		}
		if len(p.Addr) == 0 {
			return fmt.Errorf("federation: peer_exchange.peers[%d].addr empty", i)
		}
		if len(p.Addr) > MaxKnownPeerAddrLen {
			return fmt.Errorf("federation: peer_exchange.peers[%d].addr len %d > %d", i, len(p.Addr), MaxKnownPeerAddrLen)
		}
		if !utf8.ValidString(p.Addr) {
			return fmt.Errorf("federation: peer_exchange.peers[%d].addr invalid utf-8", i)
		}
		if len(p.RegionHint) > MaxKnownPeerRegionHintLen {
			return fmt.Errorf("federation: peer_exchange.peers[%d].region_hint len %d > %d", i, len(p.RegionHint), MaxKnownPeerRegionHintLen)
		}
		if !utf8.ValidString(p.RegionHint) {
			return fmt.Errorf("federation: peer_exchange.peers[%d].region_hint invalid utf-8", i)
		}
		if math.IsNaN(p.HealthScore) || p.HealthScore < 0.0 || p.HealthScore > 1.0 {
			return fmt.Errorf("federation: peer_exchange.peers[%d].health_score %f out of [0,1]", i, p.HealthScore)
		}
	}
	return nil
}

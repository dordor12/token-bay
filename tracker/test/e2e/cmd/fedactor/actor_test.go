package main

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/sha256"
	"testing"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/tracker/internal/federation"
	"google.golang.org/protobuf/proto"
)

func newTestActor(t *testing.T) *Actor {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(crand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewActor(priv)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestFedIDDerivation asserts the actor's tracker_id is sha256(pubkey), the
// value that must appear as Envelope.sender_id on every message it emits.
func TestFedIDDerivation(t *testing.T) {
	a := newTestActor(t)
	want := sha256.Sum256(a.pub)
	got := a.FedID().Bytes()
	if got != want {
		t.Fatalf("fedID = %x, want sha256(pub) = %x", got, want)
	}
}

// TestRootAttestationFrameValid proves the actor emits a wire-valid, signed
// ROOT_ATTESTATION: UnmarshalFrame round-trips it, sender_id == fedID, and the
// envelope signature verifies under the fed pubkey.
func TestRootAttestationFrameValid(t *testing.T) {
	a := newTestActor(t)
	root := make([]byte, fed.RootLen)
	for i := range root {
		root[i] = byte(i + 1)
	}

	frame, err := a.buildRootAttestationFrame(7, root)
	if err != nil {
		t.Fatal(err)
	}
	env, err := federation.UnmarshalFrame(frame)
	if err != nil {
		t.Fatalf("UnmarshalFrame: %v", err)
	}
	if env.Kind != fed.Kind_KIND_ROOT_ATTESTATION {
		t.Fatalf("kind = %v, want ROOT_ATTESTATION", env.Kind)
	}
	fedID := a.FedID().Bytes()
	if string(env.SenderId) != string(fedID[:]) {
		t.Fatalf("sender_id = %x, want fedID %x", env.SenderId, fedID)
	}
	if err := federation.VerifyEnvelope(a.pub, env); err != nil {
		t.Fatalf("envelope sig does not verify: %v", err)
	}

	var ra fed.RootAttestation
	if err := proto.Unmarshal(env.Payload, &ra); err != nil {
		t.Fatal(err)
	}
	if ra.Hour != 7 || string(ra.MerkleRoot) != string(root) {
		t.Fatalf("payload mismatch: hour=%d root=%x", ra.Hour, ra.MerkleRoot)
	}
}

// TestEquivocatingRootsDiffer proves the two frames used to trigger the
// victim's equivocation path carry the SAME tracker_id + hour but DIFFERENT
// merkle roots — the exact condition PutPeerRoot flags as a conflict.
func TestEquivocatingRootsDiffer(t *testing.T) {
	a := newTestActor(t)
	rootA := make([]byte, fed.RootLen)
	rootB := make([]byte, fed.RootLen)
	for i := range rootA {
		rootA[i] = 0xAA
		rootB[i] = 0xBB
	}

	fA, err := a.buildRootAttestationFrame(9, rootA)
	if err != nil {
		t.Fatal(err)
	}
	fB, err := a.buildRootAttestationFrame(9, rootB)
	if err != nil {
		t.Fatal(err)
	}

	eA, _ := federation.UnmarshalFrame(fA)
	eB, _ := federation.UnmarshalFrame(fB)
	var raA, raB fed.RootAttestation
	if err := proto.Unmarshal(eA.Payload, &raA); err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(eB.Payload, &raB); err != nil {
		t.Fatal(err)
	}
	if string(raA.TrackerId) != string(raB.TrackerId) {
		t.Fatal("tracker_id differs between the two roots")
	}
	if raA.Hour != raB.Hour {
		t.Fatal("hour differs between the two roots")
	}
	if string(raA.MerkleRoot) == string(raB.MerkleRoot) {
		t.Fatal("merkle roots are equal — no equivocation would be detected")
	}
}

// TestRevocationFrameValid proves the actor emits a wire-valid, signed
// REVOCATION whose tracker_sig verifies against CanonicalRevocationPreSig
// under the fed pubkey — the signature the victim actually checks.
func TestRevocationFrameValid(t *testing.T) {
	a := newTestActor(t)
	identity := make([]byte, fed.TrackerIDLen)
	for i := range identity {
		identity[i] = byte(0x40 + i)
	}

	frame, err := a.buildRevocationFrame(identity, int(fed.RevocationReason_REVOCATION_REASON_ABUSE))
	if err != nil {
		t.Fatal(err)
	}
	env, err := federation.UnmarshalFrame(frame)
	if err != nil {
		t.Fatalf("UnmarshalFrame: %v", err)
	}
	if env.Kind != fed.Kind_KIND_REVOCATION {
		t.Fatalf("kind = %v, want REVOCATION", env.Kind)
	}
	fedID := a.FedID().Bytes()
	if string(env.SenderId) != string(fedID[:]) {
		t.Fatalf("sender_id = %x, want fedID %x", env.SenderId, fedID)
	}
	if err := federation.VerifyEnvelope(a.pub, env); err != nil {
		t.Fatalf("envelope sig does not verify: %v", err)
	}

	var rev fed.Revocation
	if err := proto.Unmarshal(env.Payload, &rev); err != nil {
		t.Fatal(err)
	}
	if err := fed.ValidateRevocation(&rev); err != nil {
		t.Fatalf("revocation shape invalid: %v", err)
	}
	canonical, err := fed.CanonicalRevocationPreSig(&rev)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(a.pub, canonical, rev.TrackerSig) {
		t.Fatal("revocation tracker_sig does not verify under fed pubkey")
	}
	if string(rev.TrackerId) != string(fedID[:]) {
		t.Fatalf("revocation issuer = %x, want fedID %x", rev.TrackerId, fedID)
	}
}

// TestEquivocationEvidenceFrameValid proves a directly-emitted
// EQUIVOCATION_EVIDENCE is wire-valid and signed.
func TestEquivocationEvidenceFrameValid(t *testing.T) {
	a := newTestActor(t)
	rootA := make([]byte, fed.RootLen)
	rootB := make([]byte, fed.RootLen)
	for i := range rootA {
		rootA[i] = 0x11
		rootB[i] = 0x22
	}
	frame, err := a.buildEquivocationFrame(3, rootA, rootB)
	if err != nil {
		t.Fatal(err)
	}
	env, err := federation.UnmarshalFrame(frame)
	if err != nil {
		t.Fatalf("UnmarshalFrame: %v", err)
	}
	if env.Kind != fed.Kind_KIND_EQUIVOCATION_EVIDENCE {
		t.Fatalf("kind = %v, want EQUIVOCATION_EVIDENCE", env.Kind)
	}
	if err := federation.VerifyEnvelope(a.pub, env); err != nil {
		t.Fatalf("envelope sig does not verify: %v", err)
	}
	var evi fed.EquivocationEvidence
	if err := proto.Unmarshal(env.Payload, &evi); err != nil {
		t.Fatal(err)
	}
	if err := fed.ValidateEquivocationEvidence(&evi); err != nil {
		t.Fatalf("evidence shape invalid: %v", err)
	}
}

// TestSendBeforeHandshakeFails guards the "not connected" path.
func TestSendBeforeHandshakeFails(t *testing.T) {
	a := newTestActor(t)
	root := make([]byte, fed.RootLen)
	if err := a.sendRootAttestation(context.Background(), 1, hexOf(root)); err == nil {
		t.Fatal("expected error sending before handshake")
	}
}

func hexOf(b []byte) string {
	const hextable = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hextable[v>>4]
		out[i*2+1] = hextable[v&0x0f]
	}
	return string(out)
}

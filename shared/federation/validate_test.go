package federation_test

import (
	"strings"
	"testing"

	fed "github.com/token-bay/token-bay/shared/federation"
)

func b(n int, fill byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = fill
	}
	return out
}

func TestValidateEnvelope_Valid(t *testing.T) {
	t.Parallel()
	if err := fed.ValidateEnvelope(&fed.Envelope{
		SenderId:  b(32, 1),
		Kind:      fed.Kind_KIND_PING,
		Payload:   []byte{0x01},
		SenderSig: b(64, 2),
	}); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestValidateEnvelope_Errors(t *testing.T) {
	t.Parallel()
	cases := map[string]*fed.Envelope{
		"nil":           nil,
		"sender_id_len": {SenderId: b(31, 1), Kind: fed.Kind_KIND_PING, Payload: []byte{1}, SenderSig: b(64, 2)},
		"kind_zero":     {SenderId: b(32, 1), Kind: fed.Kind_KIND_UNSPECIFIED, Payload: []byte{1}, SenderSig: b(64, 2)},
		"kind_oob":      {SenderId: b(32, 1), Kind: fed.Kind(999), Payload: []byte{1}, SenderSig: b(64, 2)},
		"payload_empty": {SenderId: b(32, 1), Kind: fed.Kind_KIND_PING, Payload: nil, SenderSig: b(64, 2)},
		"sig_len":       {SenderId: b(32, 1), Kind: fed.Kind_KIND_PING, Payload: []byte{1}, SenderSig: b(63, 2)},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			if err := fed.ValidateEnvelope(env); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestValidateRootAttestation_Valid(t *testing.T) {
	t.Parallel()
	if err := fed.ValidateRootAttestation(&fed.RootAttestation{
		TrackerId:  b(32, 1),
		Hour:       42,
		MerkleRoot: b(32, 3),
		TrackerSig: b(64, 4),
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRootAttestation_Errors(t *testing.T) {
	t.Parallel()
	cases := map[string]*fed.RootAttestation{
		"nil":                 nil,
		"tracker_id_len":      {TrackerId: b(31, 1), Hour: 1, MerkleRoot: b(32, 3), TrackerSig: b(64, 4)},
		"tracker_id_len_only": {TrackerId: b(31, 1), Hour: 1, MerkleRoot: b(32, 3), TrackerSig: b(64, 4)},
		"tracker_id_zero":     {TrackerId: make([]byte, 32), Hour: 1, MerkleRoot: b(32, 3), TrackerSig: b(64, 4)},
		"hour_zero":           {TrackerId: b(32, 1), Hour: 0, MerkleRoot: b(32, 3), TrackerSig: b(64, 4)},
		"root_len":            {TrackerId: b(32, 1), Hour: 1, MerkleRoot: b(31, 3), TrackerSig: b(64, 4)},
		"sig_len":             {TrackerId: b(32, 1), Hour: 1, MerkleRoot: b(32, 3), TrackerSig: b(63, 4)},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			if err := fed.ValidateRootAttestation(m); err == nil || !strings.Contains(err.Error(), "federation:") {
				t.Fatalf("expected federation: error for %s, got %v", name, err)
			}
		})
	}
}

func TestValidateEquivocationEvidence_Valid(t *testing.T) {
	t.Parallel()
	if err := fed.ValidateEquivocationEvidence(&fed.EquivocationEvidence{
		TrackerId: b(32, 1), Hour: 7,
		RootA: b(32, 9), SigA: b(64, 9),
		RootB: b(32, 8), SigB: b(64, 8),
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateEquivocationEvidence_Errors(t *testing.T) {
	t.Parallel()
	cases := map[string]*fed.EquivocationEvidence{
		"nil":         nil,
		"roots_equal": {TrackerId: b(32, 1), Hour: 1, RootA: b(32, 9), SigA: b(64, 9), RootB: b(32, 9), SigB: b(64, 9)},
		"root_a_len":  {TrackerId: b(32, 1), Hour: 1, RootA: b(31, 9), SigA: b(64, 9), RootB: b(32, 8), SigB: b(64, 8)},
		"root_b_len":  {TrackerId: b(32, 1), Hour: 1, RootA: b(32, 9), SigA: b(64, 9), RootB: b(31, 8), SigB: b(64, 8)},
		"sig_a_len":   {TrackerId: b(32, 1), Hour: 1, RootA: b(32, 9), SigA: b(63, 9), RootB: b(32, 8), SigB: b(64, 8)},
		"sig_b_len":   {TrackerId: b(32, 1), Hour: 1, RootA: b(32, 9), SigA: b(64, 9), RootB: b(32, 8), SigB: b(63, 8)},
	}
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			if err := fed.ValidateEquivocationEvidence(m); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestValidateHelloAndPeerAuth(t *testing.T) {
	t.Parallel()
	if err := fed.ValidateHello(&fed.Hello{TrackerId: b(32, 1), ProtocolVersion: 1, Nonce: b(32, 2)}); err != nil {
		t.Fatalf("hello valid: %v", err)
	}
	if err := fed.ValidateHello(&fed.Hello{TrackerId: b(32, 1), ProtocolVersion: 1, Nonce: b(31, 2)}); err == nil {
		t.Fatal("expected nonce-len error")
	}
	if err := fed.ValidatePeerAuth(&fed.PeerAuth{NonceSig: b(64, 1)}); err != nil {
		t.Fatalf("peerauth valid: %v", err)
	}
	if err := fed.ValidatePeerAuth(&fed.PeerAuth{NonceSig: b(63, 1)}); err == nil {
		t.Fatal("expected sig-len error")
	}
}

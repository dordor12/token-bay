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

func TestValidateEnvelope_AcceptsTransferKinds(t *testing.T) {
	t.Parallel()
	for _, k := range []fed.Kind{
		fed.Kind_KIND_TRANSFER_PROOF_REQUEST,
		fed.Kind_KIND_TRANSFER_PROOF,
		fed.Kind_KIND_TRANSFER_APPLIED,
	} {
		if err := fed.ValidateEnvelope(&fed.Envelope{
			SenderId:  b(32, 1),
			Kind:      k,
			Payload:   []byte{0x01},
			SenderSig: b(64, 2),
		}); err != nil {
			t.Fatalf("kind=%v: ValidateEnvelope err=%v, want nil", k, err)
		}
	}
}

func validTransferProofRequest() *fed.TransferProofRequest {
	return &fed.TransferProofRequest{
		SourceTrackerId: b(32, 0x11),
		DestTrackerId:   b(32, 0x22),
		IdentityId:      b(32, 0x33),
		Amount:          1000,
		Nonce:           b(32, 0x44),
		ConsumerSig:     b(64, 0x55),
		ConsumerPub:     b(32, 0x66),
		Timestamp:       1714000000,
	}
}

func TestValidateTransferProofRequest_Valid(t *testing.T) {
	t.Parallel()
	if err := fed.ValidateTransferProofRequest(validTransferProofRequest()); err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
}

func TestValidateTransferProofRequest_Errors(t *testing.T) {
	t.Parallel()
	cases := map[string]func(m *fed.TransferProofRequest){
		"src_len":          func(m *fed.TransferProofRequest) { m.SourceTrackerId = b(4, 1) },
		"dst_len":          func(m *fed.TransferProofRequest) { m.DestTrackerId = b(4, 1) },
		"identity_len":     func(m *fed.TransferProofRequest) { m.IdentityId = b(4, 1) },
		"amount_zero":      func(m *fed.TransferProofRequest) { m.Amount = 0 },
		"nonce_len":        func(m *fed.TransferProofRequest) { m.Nonce = b(4, 1) },
		"consumer_sig_len": func(m *fed.TransferProofRequest) { m.ConsumerSig = b(4, 1) },
		"consumer_pub_len": func(m *fed.TransferProofRequest) { m.ConsumerPub = b(4, 1) },
		"src_eq_dst":       func(m *fed.TransferProofRequest) { m.DestTrackerId = m.SourceTrackerId },
		"src_zero":         func(m *fed.TransferProofRequest) { m.SourceTrackerId = make([]byte, 32) },
		"dst_zero":         func(m *fed.TransferProofRequest) { m.DestTrackerId = make([]byte, 32) },
		"identity_zero":    func(m *fed.TransferProofRequest) { m.IdentityId = make([]byte, 32) },
		"nonce_zero":       func(m *fed.TransferProofRequest) { m.Nonce = make([]byte, 32) },
		"timestamp_zero":   func(m *fed.TransferProofRequest) { m.Timestamp = 0 },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			m := validTransferProofRequest()
			mut(m)
			if err := fed.ValidateTransferProofRequest(m); err == nil {
				t.Fatalf("err=nil, want error")
			}
		})
	}
	if err := fed.ValidateTransferProofRequest(nil); err == nil {
		t.Fatal("nil: err=nil, want error")
	}
}

func validTransferProof() *fed.TransferProof {
	return &fed.TransferProof{
		SourceTrackerId:    b(32, 0x11),
		DestTrackerId:      b(32, 0x22),
		IdentityId:         b(32, 0x33),
		Amount:             1000,
		Nonce:              b(32, 0x44),
		SourceChainTipHash: b(32, 0x55),
		SourceSeq:          42,
		Timestamp:          1714000000,
		SourceTrackerSig:   b(64, 0x66),
	}
}

func TestValidateTransferProof_Valid(t *testing.T) {
	t.Parallel()
	if err := fed.ValidateTransferProof(validTransferProof()); err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
}

func TestValidateTransferProof_Errors(t *testing.T) {
	t.Parallel()
	cases := map[string]func(m *fed.TransferProof){
		"src_len":        func(m *fed.TransferProof) { m.SourceTrackerId = b(4, 1) },
		"dst_len":        func(m *fed.TransferProof) { m.DestTrackerId = b(4, 1) },
		"identity_len":   func(m *fed.TransferProof) { m.IdentityId = b(4, 1) },
		"amount_zero":    func(m *fed.TransferProof) { m.Amount = 0 },
		"nonce_len":      func(m *fed.TransferProof) { m.Nonce = b(4, 1) },
		"chain_tip_len":  func(m *fed.TransferProof) { m.SourceChainTipHash = b(4, 1) },
		"sig_len":        func(m *fed.TransferProof) { m.SourceTrackerSig = b(4, 1) },
		"src_eq_dst":     func(m *fed.TransferProof) { m.DestTrackerId = m.SourceTrackerId },
		"timestamp_zero": func(m *fed.TransferProof) { m.Timestamp = 0 },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			m := validTransferProof()
			mut(m)
			if err := fed.ValidateTransferProof(m); err == nil {
				t.Fatalf("err=nil, want error")
			}
		})
	}
	if err := fed.ValidateTransferProof(nil); err == nil {
		t.Fatal("nil: err=nil, want error")
	}
}

func validTransferApplied() *fed.TransferApplied {
	return &fed.TransferApplied{
		SourceTrackerId: b(32, 0x11),
		DestTrackerId:   b(32, 0x22),
		Nonce:           b(32, 0x44),
		Timestamp:       1714000000,
		DestTrackerSig:  b(64, 0x66),
	}
}

func TestValidateTransferApplied_Valid(t *testing.T) {
	t.Parallel()
	if err := fed.ValidateTransferApplied(validTransferApplied()); err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
}

func TestValidateTransferApplied_Errors(t *testing.T) {
	t.Parallel()
	cases := map[string]func(m *fed.TransferApplied){
		"src_len":        func(m *fed.TransferApplied) { m.SourceTrackerId = b(4, 1) },
		"dst_len":        func(m *fed.TransferApplied) { m.DestTrackerId = b(4, 1) },
		"nonce_len":      func(m *fed.TransferApplied) { m.Nonce = b(4, 1) },
		"sig_len":        func(m *fed.TransferApplied) { m.DestTrackerSig = b(4, 1) },
		"src_eq_dst":     func(m *fed.TransferApplied) { m.DestTrackerId = m.SourceTrackerId },
		"timestamp_zero": func(m *fed.TransferApplied) { m.Timestamp = 0 },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			m := validTransferApplied()
			mut(m)
			if err := fed.ValidateTransferApplied(m); err == nil {
				t.Fatalf("err=nil, want error")
			}
		})
	}
	if err := fed.ValidateTransferApplied(nil); err == nil {
		t.Fatal("nil: err=nil, want error")
	}
}

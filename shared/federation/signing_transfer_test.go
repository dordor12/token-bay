package federation_test

import (
	"bytes"
	"testing"

	fed "github.com/token-bay/token-bay/shared/federation"
)

func TestCanonicalTransferProofRequestPreSig_Deterministic(t *testing.T) {
	t.Parallel()
	a, err := fed.CanonicalTransferProofRequestPreSig(validTransferProofRequest())
	if err != nil {
		t.Fatal(err)
	}
	b2, err := fed.CanonicalTransferProofRequestPreSig(validTransferProofRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b2) {
		t.Fatalf("canonical bytes differ between equal messages\n a=%x\n b=%x", a, b2)
	}
}

func TestCanonicalTransferProofRequestPreSig_ZeroesConsumerSig(t *testing.T) {
	t.Parallel()
	a, err := fed.CanonicalTransferProofRequestPreSig(validTransferProofRequest())
	if err != nil {
		t.Fatal(err)
	}
	tampered := validTransferProofRequest()
	tampered.ConsumerSig = b(64, 0xFF)
	c, err := fed.CanonicalTransferProofRequestPreSig(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, c) {
		t.Fatal("changing consumer_sig changed canonical bytes — pre-sig must zero it")
	}
}

func TestCanonicalTransferProofRequestPreSig_DetectsTampering(t *testing.T) {
	t.Parallel()
	a, err := fed.CanonicalTransferProofRequestPreSig(validTransferProofRequest())
	if err != nil {
		t.Fatal(err)
	}
	tampered := validTransferProofRequest()
	tampered.Amount = tampered.Amount + 1
	c, err := fed.CanonicalTransferProofRequestPreSig(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, c) {
		t.Fatal("tampering Amount produced identical canonical bytes")
	}
}

func TestCanonicalTransferProofRequestPreSig_NilMessage(t *testing.T) {
	t.Parallel()
	if _, err := fed.CanonicalTransferProofRequestPreSig(nil); err == nil {
		t.Fatal("err=nil, want error")
	}
}

func TestCanonicalTransferProofPreSig_ZeroesSourceSig(t *testing.T) {
	t.Parallel()
	a, err := fed.CanonicalTransferProofPreSig(validTransferProof())
	if err != nil {
		t.Fatal(err)
	}
	tampered := validTransferProof()
	tampered.SourceTrackerSig = b(64, 0xFF)
	c, err := fed.CanonicalTransferProofPreSig(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, c) {
		t.Fatal("changing source_tracker_sig changed canonical bytes — pre-sig must zero it")
	}
}

func TestCanonicalTransferProofPreSig_DetectsAmountTampering(t *testing.T) {
	t.Parallel()
	a, err := fed.CanonicalTransferProofPreSig(validTransferProof())
	if err != nil {
		t.Fatal(err)
	}
	bad := validTransferProof()
	bad.Amount = 999999
	c, err := fed.CanonicalTransferProofPreSig(bad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, c) {
		t.Fatal("Amount tamper produced identical canonical bytes")
	}
}

func TestCanonicalTransferProofPreSig_NilMessage(t *testing.T) {
	t.Parallel()
	if _, err := fed.CanonicalTransferProofPreSig(nil); err == nil {
		t.Fatal("err=nil, want error")
	}
}

func TestCanonicalTransferAppliedPreSig_ZeroesDestSig(t *testing.T) {
	t.Parallel()
	a, err := fed.CanonicalTransferAppliedPreSig(validTransferApplied())
	if err != nil {
		t.Fatal(err)
	}
	tampered := validTransferApplied()
	tampered.DestTrackerSig = b(64, 0xFF)
	c, err := fed.CanonicalTransferAppliedPreSig(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, c) {
		t.Fatal("changing dest_tracker_sig changed canonical bytes — pre-sig must zero it")
	}
}

func TestCanonicalTransferAppliedPreSig_DetectsNonceTampering(t *testing.T) {
	t.Parallel()
	a, err := fed.CanonicalTransferAppliedPreSig(validTransferApplied())
	if err != nil {
		t.Fatal(err)
	}
	bad := validTransferApplied()
	bad.Nonce[0] ^= 0xFF
	c, err := fed.CanonicalTransferAppliedPreSig(bad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, c) {
		t.Fatal("Nonce tamper produced identical canonical bytes")
	}
}

func TestCanonicalTransferAppliedPreSig_NilMessage(t *testing.T) {
	t.Parallel()
	if _, err := fed.CanonicalTransferAppliedPreSig(nil); err == nil {
		t.Fatal("err=nil, want error")
	}
}

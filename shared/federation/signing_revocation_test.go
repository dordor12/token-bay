package federation_test

import (
	"bytes"
	"testing"

	fed "github.com/token-bay/token-bay/shared/federation"
)

func TestCanonicalRevocationPreSig_Deterministic(t *testing.T) {
	t.Parallel()
	a, err := fed.CanonicalRevocationPreSig(validRevocation())
	if err != nil {
		t.Fatal(err)
	}
	b2, err := fed.CanonicalRevocationPreSig(validRevocation())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b2) {
		t.Fatalf("canonical bytes differ between equal messages\n a=%x\n b=%x", a, b2)
	}
}

func TestCanonicalRevocationPreSig_ZeroesTrackerSig(t *testing.T) {
	t.Parallel()
	a, err := fed.CanonicalRevocationPreSig(validRevocation())
	if err != nil {
		t.Fatal(err)
	}
	tampered := validRevocation()
	tampered.TrackerSig = b(64, 0xFF)
	c, err := fed.CanonicalRevocationPreSig(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, c) {
		t.Fatal("changing tracker_sig changed canonical bytes — pre-sig must zero it")
	}
}

func TestCanonicalRevocationPreSig_DoesNotMutate(t *testing.T) {
	t.Parallel()
	m := validRevocation()
	want := b(64, 0xAB)
	m.TrackerSig = append([]byte(nil), want...)
	if _, err := fed.CanonicalRevocationPreSig(m); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(m.TrackerSig, want) {
		t.Fatalf("input mutated: got=%x want=%x", m.TrackerSig, want)
	}
}

func TestCanonicalRevocationPreSig_TamperDetect(t *testing.T) {
	t.Parallel()
	a, err := fed.CanonicalRevocationPreSig(validRevocation())
	if err != nil {
		t.Fatal(err)
	}
	tampered := validRevocation()
	tampered.RevokedAt = 9999999999
	c, err := fed.CanonicalRevocationPreSig(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, c) {
		t.Fatal("revoked_at changed but canonical bytes did not")
	}
}

func TestCanonicalRevocationPreSig_Nil(t *testing.T) {
	t.Parallel()
	if _, err := fed.CanonicalRevocationPreSig(nil); err == nil {
		t.Fatal("err=nil, want error")
	}
}

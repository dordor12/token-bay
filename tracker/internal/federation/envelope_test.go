package federation_test

import (
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/sha256"
	"testing"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/tracker/internal/federation"
	"google.golang.org/protobuf/proto"
)

func TestEnvelope_SignAndVerify_RoundTrip(t *testing.T) {
	t.Parallel()
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	id := sha256.Sum256(pub)

	payload, _ := proto.Marshal(&fed.Ping{Nonce: 99})
	env, err := federation.SignEnvelope(priv, id[:], fed.Kind_KIND_PING, payload)
	if err != nil {
		t.Fatal(err)
	}
	if got := federation.MessageID(env); got != sha256.Sum256(payload) {
		t.Fatalf("MessageID mismatch: %x vs %x", got, sha256.Sum256(payload))
	}
	if err := federation.VerifyEnvelope(pub, env); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestEnvelope_VerifyEnvelope_RejectsBadSig(t *testing.T) {
	t.Parallel()
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	other, _, _ := ed25519.GenerateKey(crand.Reader)
	id := sha256.Sum256(pub)
	env, _ := federation.SignEnvelope(priv, id[:], fed.Kind_KIND_PING, []byte{1})
	if err := federation.VerifyEnvelope(other, env); err == nil {
		t.Fatal("expected verify failure under wrong key")
	}
}

func TestEnvelope_MarshalUnmarshalFrame(t *testing.T) {
	t.Parallel()
	pub, priv, _ := ed25519.GenerateKey(crand.Reader)
	id := sha256.Sum256(pub)
	env, _ := federation.SignEnvelope(priv, id[:], fed.Kind_KIND_PING, []byte{1, 2, 3})
	frame, err := federation.MarshalFrame(env)
	if err != nil {
		t.Fatal(err)
	}
	got, err := federation.UnmarshalFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != fed.Kind_KIND_PING || string(got.Payload) != string([]byte{1, 2, 3}) {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

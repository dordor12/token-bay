package api_test

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/api"
)

func TestPushAPI_OfferPush_RoundTrip(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{})
	pa := r.PushAPI()
	in := &tbproto.OfferPush{
		ConsumerId:      bytes.Repeat([]byte{0xab}, 32),
		EnvelopeHash:    bytes.Repeat([]byte{0xcd}, 32),
		Model:           "claude-opus-4-7",
		MaxInputTokens:  4096,
		MaxOutputTokens: 1024,
	}
	b, err := pa.EncodeOfferPush(in)
	if err != nil {
		t.Fatal(err)
	}
	var got tbproto.OfferPush
	if err := proto.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != in.Model || got.MaxInputTokens != in.MaxInputTokens {
		t.Fatalf("round-trip lost data: %+v", &got)
	}
}

func TestPushAPI_OfferPush_RejectsInvalid(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{})
	_, err := r.PushAPI().EncodeOfferPush(&tbproto.OfferPush{})
	if err == nil {
		t.Fatal("want validation error on empty OfferPush")
	}
}

func TestPushAPI_DecodeOfferDecision_Accept(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{})
	pa := r.PushAPI()
	src := &tbproto.OfferDecision{
		Accept:          true,
		EphemeralPubkey: bytes.Repeat([]byte{0x11}, 32),
	}
	b, _ := proto.Marshal(src)
	got, err := pa.DecodeOfferDecision(b)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Accept || len(got.EphemeralPubkey) != 32 {
		t.Fatalf("got %+v", got)
	}
}

func TestPushAPI_DecodeOfferDecision_RejectsEmpty(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{})
	_, err := r.PushAPI().DecodeOfferDecision(nil)
	if err == nil {
		t.Fatal("want error on empty bytes")
	}
}

func TestPushAPI_DecodeOfferDecision_RejectsMalformed(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{})
	_, err := r.PushAPI().DecodeOfferDecision([]byte{0xff, 0xff, 0xff})
	if err == nil {
		t.Fatal("want unmarshal error on garbage bytes")
	}
}

func TestPushAPI_SettlementPush_RoundTrip(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{})
	pa := r.PushAPI()
	in := &tbproto.SettlementPush{
		PreimageHash: bytes.Repeat([]byte{0x22}, 32),
		PreimageBody: []byte("body"),
	}
	b, err := pa.EncodeSettlementPush(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		t.Fatal("empty encode")
	}
}

func TestPushAPI_SettlementPush_RejectsInvalid(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{})
	_, err := r.PushAPI().EncodeSettlementPush(&tbproto.SettlementPush{})
	if err == nil {
		t.Fatal("want validation error on empty SettlementPush")
	}
}

func TestPushAPI_DecodeSettleAck_OK(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{})
	src := &tbproto.SettleAck{}
	b, _ := proto.Marshal(src)
	if _, err := r.PushAPI().DecodeSettleAck(b); err != nil {
		t.Fatal(err)
	}
}

func TestPushAPI_TagConstants(t *testing.T) {
	if api.PushTagOffer != 0x01 {
		t.Errorf("PushTagOffer = %#x, want 0x01", api.PushTagOffer)
	}
	if api.PushTagSettlement != 0x02 {
		t.Errorf("PushTagSettlement = %#x, want 0x02", api.PushTagSettlement)
	}
}

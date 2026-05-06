package api

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)

// Push-stream class tags. The server writes one of these as a single
// byte on a fresh server-initiated bidi stream before the framed push
// proto. Mirror of the merged plugin
// trackerclient/push_settlements.go:13-16.
const (
	PushTagOffer      byte = 0x01
	PushTagSettlement byte = 0x02
)

// PushAPI provides encode/decode helpers for the two push-stream
// message pairs. server uses these — handlers don't.
type PushAPI struct{}

// EncodeOfferPush serializes push deterministically. Validates first so
// callers (server.PushOfferTo) get fast-fail on missing fields.
func (PushAPI) EncodeOfferPush(push *tbproto.OfferPush) ([]byte, error) {
	if err := tbproto.ValidateOfferPush(push); err != nil {
		return nil, err
	}
	return signing.DeterministicMarshal(push)
}

// EncodeSettlementPush mirrors EncodeOfferPush.
func (PushAPI) EncodeSettlementPush(push *tbproto.SettlementPush) ([]byte, error) {
	if err := tbproto.ValidateSettlementPush(push); err != nil {
		return nil, err
	}
	return signing.DeterministicMarshal(push)
}

// DecodeOfferDecision unmarshals the consumer's reply to an offer push.
// Empty bytes are rejected so callers distinguish "no reply" from
// "explicit zero-value reply".
func (PushAPI) DecodeOfferDecision(b []byte) (*tbproto.OfferDecision, error) {
	if len(b) == 0 {
		return nil, errors.New("api: empty OfferDecision bytes")
	}
	var dec tbproto.OfferDecision
	if err := proto.Unmarshal(b, &dec); err != nil {
		return nil, fmt.Errorf("api: unmarshal OfferDecision: %w", err)
	}
	return &dec, nil
}

// DecodeSettleAck unmarshals the consumer's reply to a settlement push.
// Empty proto is permitted (SettleAck has no fields).
func (PushAPI) DecodeSettleAck(b []byte) (*tbproto.SettleAck, error) {
	var ack tbproto.SettleAck
	if err := proto.Unmarshal(b, &ack); err != nil {
		return nil, fmt.Errorf("api: unmarshal SettleAck: %w", err)
	}
	return &ack, nil
}

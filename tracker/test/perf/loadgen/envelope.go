//go:build perf

package loadgen

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/exhaustionproof"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
	"github.com/token-bay/token-bay/tracker/test/e2e/driver"
)

// buildSignedBrokerEnvelope assembles a fully valid, consumer-signed
// broker_request payload — the same construction as the e2e suite's
// helper (tracker/test/e2e/coverage_rpc_test.go), reproduced from
// shared packages only, minus the *testing.T plumbing. The ephemeral
// pubkey is random: simulated participants never dial the tunnel it
// would pin.
func buildSignedBrokerEnvelope(cli *driver.RPCClient, model string, maxIn, maxOut uint64, snap *tbproto.SignedBalanceSnapshot) ([]byte, error) {
	ts := uint64(time.Now().Unix()) //nolint:gosec // unix seconds, always positive
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("loadgen: envelope nonce: %w", err)
	}
	proofNonce := make([]byte, 16)
	if _, err := rand.Read(proofNonce); err != nil {
		return nil, fmt.Errorf("loadgen: proof nonce: %w", err)
	}
	ephPub := make([]byte, ed25519.PublicKeySize)
	if _, err := rand.Read(ephPub); err != nil {
		return nil, fmt.Errorf("loadgen: ephemeral pub: %w", err)
	}

	id := cli.IdentityID()
	body := &tbproto.EnvelopeBody{
		ProtocolVersion:      uint32(tbproto.ProtocolVersion),
		ConsumerId:           id[:],
		Model:                model,
		MaxInputTokens:       maxIn,
		MaxOutputTokens:      maxOut,
		Tier:                 tbproto.PrivacyTier_PRIVACY_TIER_STANDARD,
		BodyHash:             make([]byte, 32),
		ConsumerEphemeralPub: ephPub,
		ExhaustionProof: &exhaustionproof.ExhaustionProofV1{
			StopFailure: &exhaustionproof.StopFailure{Matcher: "rate_limit", At: ts},
			UsageProbe:  &exhaustionproof.UsageProbe{At: ts},
			CapturedAt:  ts,
			Nonce:       proofNonce,
		},
		BalanceProof: snap,
		CapturedAt:   ts,
		Nonce:        nonce,
	}
	sig, err := signing.SignEnvelope(cli.PrivateKey(), body)
	if err != nil {
		return nil, fmt.Errorf("loadgen: sign envelope: %w", err)
	}
	payload, err := proto.Marshal(&tbproto.EnvelopeSigned{Body: body, ConsumerSig: sig})
	if err != nil {
		return nil, fmt.Errorf("loadgen: marshal envelope: %w", err)
	}
	return payload, nil
}

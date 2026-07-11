package signing

import (
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"
)

// usageAssertionDomain namespaces the usage-assertion preimage so a signature
// over it can never be replayed as any other Token-Bay message.
const usageAssertionDomain = "token-bay/usage-assertion:v1"

// UsageAssertion is the sequencing-independent settlement authorization both
// the seeder (in UsageReport) and the consumer (counter-sig) sign. It omits
// prev_hash/seq/timestamp/flags — those are ledger sequencing the tracker
// assigns at append time.
type UsageAssertion struct {
	RequestID    []byte // 16
	ConsumerID   []byte // 32
	SeederID     []byte // 32
	Model        string
	InputTokens  uint32
	OutputTokens uint32
	CostCredits  uint64
}

func CanonicalUsageAssertionPreSig(a UsageAssertion) ([]byte, error) {
	if len(a.RequestID) != 16 {
		return nil, errors.New("signing: usage assertion request_id must be 16 bytes")
	}
	if len(a.ConsumerID) != 32 || len(a.SeederID) != 32 {
		return nil, errors.New("signing: usage assertion consumer_id/seeder_id must be 32 bytes")
	}
	if a.Model == "" {
		return nil, errors.New("signing: usage assertion model empty")
	}
	// domain \0 request_id \0 consumer_id \0 seeder_id \0 model \0 in(8) out(8) cost(8)
	var buf []byte
	buf = append(buf, usageAssertionDomain...)
	buf = append(buf, 0)
	buf = append(buf, a.RequestID...)
	buf = append(buf, 0)
	buf = append(buf, a.ConsumerID...)
	buf = append(buf, 0)
	buf = append(buf, a.SeederID...)
	buf = append(buf, 0)
	buf = append(buf, a.Model...)
	buf = append(buf, 0)
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(a.InputTokens))
	buf = append(buf, n[:]...)
	binary.BigEndian.PutUint64(n[:], uint64(a.OutputTokens))
	buf = append(buf, n[:]...)
	binary.BigEndian.PutUint64(n[:], a.CostCredits)
	buf = append(buf, n[:]...)
	return buf, nil
}

func SignUsageAssertion(priv ed25519.PrivateKey, a UsageAssertion) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("signing: bad private key size %d", len(priv))
	}
	msg, err := CanonicalUsageAssertionPreSig(a)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(priv, msg), nil
}

func VerifyUsageAssertion(pub ed25519.PublicKey, a UsageAssertion, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return false
	}
	msg, err := CanonicalUsageAssertionPreSig(a)
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, msg, sig)
}

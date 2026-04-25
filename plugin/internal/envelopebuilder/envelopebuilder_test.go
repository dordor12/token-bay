package envelopebuilder

import (
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/exhaustionproof"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)

// fakeSigner is a configurable test stand-in for the production Signer.
// It is concurrency-safe via mu — TestBuild_Concurrent exercises that.
type fakeSigner struct {
	id        ids.IdentityID
	signBytes []byte // returned from Sign on success
	signErr   error  // if non-nil, Sign returns this
	mu        sync.Mutex
	calls     int
}

func (f *fakeSigner) Sign(body *tbproto.EnvelopeBody) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.signErr != nil {
		return nil, f.signErr
	}
	return f.signBytes, nil
}

func (f *fakeSigner) IdentityID() ids.IdentityID { return f.id }

func (f *fakeSigner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newFakeSigner() *fakeSigner {
	var id ids.IdentityID
	for i := range id {
		id[i] = byte(i + 1) // 0x01..0x20
	}
	sig := make([]byte, 64)
	for i := range sig {
		sig[i] = 0xAA
	}
	return &fakeSigner{id: id, signBytes: sig}
}

func fixedNow() time.Time { return time.Unix(1714000020, 0).UTC() }

func zeroRand(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// validProof returns a hand-constructed ExhaustionProofV1 that passes
// ValidateProofV1. Tests that need a valid proof start from this.
func validProof() *exhaustionproof.ExhaustionProofV1 {
	return &exhaustionproof.ExhaustionProofV1{
		StopFailure: &exhaustionproof.StopFailure{
			Matcher:    "rate_limit",
			At:         1714000000,
			ErrorShape: []byte(`{"type":"rate_limit_error"}`),
		},
		UsageProbe: &exhaustionproof.UsageProbe{
			At:     1714000005,
			Output: []byte(`Current session: 99% used`),
		},
		CapturedAt: 1714000010,
		Nonce:      bytesOfLen(16, 0xCC),
	}
}

// validBalance returns a hand-constructed *SignedBalanceSnapshot. The
// envelope builder only asserts non-nil — internal balance fields aren't
// validated by ValidateEnvelopeBody.
func validBalance() *tbproto.SignedBalanceSnapshot {
	return &tbproto.SignedBalanceSnapshot{
		Body: &tbproto.BalanceSnapshotBody{
			IdentityId:   bytesOfLen(32, 0x11),
			Credits:      100,
			ChainTipHash: bytesOfLen(32, 0x22),
			ChainTipSeq:  42,
			IssuedAt:     1714000000,
			ExpiresAt:    1714000600,
		},
		TrackerSig: bytesOfLen(64, 0x33),
	}
}

func validSpec() RequestSpec {
	return RequestSpec{
		Model:           "claude-sonnet-4-6",
		MaxInputTokens:  100_000,
		MaxOutputTokens: 8_192,
		Tier:            tbproto.PrivacyTier_PRIVACY_TIER_STANDARD,
		BodyHash:        bytesOfLen(32, 0x44),
	}
}

func bytesOfLen(n int, fill byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = fill
	}
	return out
}

func newTestBuilder(s Signer) *Builder {
	b := NewBuilder(s)
	b.Now = fixedNow
	b.RandRead = zeroRand
	return b
}

func TestBuild_HappyPath(t *testing.T) {
	signer := newFakeSigner()
	b := newTestBuilder(signer)

	env, err := b.Build(validSpec(), validProof(), validBalance())
	require.NoError(t, err)
	require.NotNil(t, env)
	require.NotNil(t, env.Body)

	// Body fields
	assert.Equal(t, uint32(tbproto.ProtocolVersion), env.Body.ProtocolVersion)
	assert.Equal(t, signer.id[:], env.Body.ConsumerId)
	assert.Equal(t, "claude-sonnet-4-6", env.Body.Model)
	assert.Equal(t, uint64(100_000), env.Body.MaxInputTokens)
	assert.Equal(t, uint64(8_192), env.Body.MaxOutputTokens)
	assert.Equal(t, tbproto.PrivacyTier_PRIVACY_TIER_STANDARD, env.Body.Tier)
	assert.Equal(t, bytesOfLen(32, 0x44), env.Body.BodyHash)
	assert.NotNil(t, env.Body.ExhaustionProof)
	assert.NotNil(t, env.Body.BalanceProof)
	assert.Equal(t, uint64(1714000020), env.Body.CapturedAt)
	assert.Len(t, env.Body.Nonce, 16)

	// Signature fed by the fake signer.
	assert.Equal(t, signer.signBytes, env.ConsumerSig)
	assert.Equal(t, 1, signer.callCount(), "Signer.Sign called exactly once")

	// Independent re-validation: builder claims valid → ValidateEnvelopeBody agrees.
	require.NoError(t, tbproto.ValidateEnvelopeBody(env.Body))
}

func TestNewBuilder_PanicsOnNilSigner(t *testing.T) {
	assert.Panics(t, func() { NewBuilder(nil) })
}

func TestNewBuilder_Defaults(t *testing.T) {
	b := NewBuilder(newFakeSigner())
	require.NotNil(t, b)
	assert.NotNil(t, b.Now)
	assert.NotNil(t, b.RandRead)
	assert.NotNil(t, b.Signer)
}

// Compile-time check: fakeSigner must satisfy Signer.
var _ Signer = (*fakeSigner)(nil)

// realEd25519Signer wraps shared/signing.SignEnvelope with a fresh keypair.
// Used by tests that exercise actual cryptographic verification.
type realEd25519Signer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
	id   ids.IdentityID
}

func newRealSigner(t *testing.T) *realEd25519Signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	// Derive IdentityID = SHA-256(pubkey). The exact derivation is irrelevant
	// to these tests (the tracker validates only that consumer_id is 32 bytes);
	// what matters is that it's deterministic per keypair.
	h := sha256.Sum256(pub)
	var id ids.IdentityID
	copy(id[:], h[:])
	return &realEd25519Signer{priv: priv, pub: pub, id: id}
}

func (r *realEd25519Signer) Sign(body *tbproto.EnvelopeBody) ([]byte, error) {
	return signing.SignEnvelope(r.priv, body)
}

func (r *realEd25519Signer) IdentityID() ids.IdentityID { return r.id }

func TestBuild_RealEd25519_RoundTrip(t *testing.T) {
	signer := newRealSigner(t)
	b := newTestBuilder(signer)

	env, err := b.Build(validSpec(), validProof(), validBalance())
	require.NoError(t, err)

	// Marshal → unmarshal — survives the wire round-trip.
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(env)
	require.NoError(t, err)

	var parsed tbproto.EnvelopeSigned
	require.NoError(t, proto.Unmarshal(wire, &parsed))

	// Real Ed25519 verification against the parsed message.
	assert.True(t, signing.VerifyEnvelope(signer.pub, &parsed),
		"VerifyEnvelope must accept a freshly-built envelope")
}

func TestBuild_RealEd25519_TamperDetected(t *testing.T) {
	signer := newRealSigner(t)
	b := newTestBuilder(signer)

	env, err := b.Build(validSpec(), validProof(), validBalance())
	require.NoError(t, err)
	require.True(t, signing.VerifyEnvelope(signer.pub, env), "sanity: original verifies")

	// Mutate the body after signing.
	env.Body.Model = "claude-haiku-4-5-20251001"
	assert.False(t, signing.VerifyEnvelope(signer.pub, env),
		"VerifyEnvelope must reject a body that has been mutated post-sign")
}

func TestBuild_RejectsNilProof(t *testing.T) {
	b := newTestBuilder(newFakeSigner())
	env, err := b.Build(validSpec(), nil, validBalance())
	assert.Nil(t, env)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNilProof), "expected ErrNilProof, got: %v", err)
}

func TestBuild_RejectsNilBalance(t *testing.T) {
	b := newTestBuilder(newFakeSigner())
	env, err := b.Build(validSpec(), validProof(), nil)
	assert.Nil(t, env)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNilBalance), "expected ErrNilBalance, got: %v", err)
}

func TestBuild_RejectsInvalidSpec(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*RequestSpec)
		wantField string
	}{
		{"empty model", func(s *RequestSpec) { s.Model = "" }, "Model"},
		{"short body_hash", func(s *RequestSpec) { s.BodyHash = bytesOfLen(31, 0x44) }, "BodyHash"},
		{"long body_hash", func(s *RequestSpec) { s.BodyHash = bytesOfLen(33, 0x44) }, "BodyHash"},
		{"unspecified tier", func(s *RequestSpec) { s.Tier = tbproto.PrivacyTier_PRIVACY_TIER_UNSPECIFIED }, "Tier"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := validSpec()
			tc.mutate(&spec)
			b := newTestBuilder(newFakeSigner())
			env, err := b.Build(spec, validProof(), validBalance())
			assert.Nil(t, env)
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidSpec),
				"expected ErrInvalidSpec, got: %v", err)
			assert.Contains(t, err.Error(), tc.wantField,
				"error should name the offending field: %v", err)
		})
	}
}

func TestBuild_SignerFailure(t *testing.T) {
	signer := newFakeSigner()
	signer.signErr = errors.New("keychain locked")
	b := newTestBuilder(signer)

	env, err := b.Build(validSpec(), validProof(), validBalance())
	assert.Nil(t, env)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSign), "expected ErrSign, got: %v", err)
	assert.Contains(t, err.Error(), "keychain locked")
}

func TestBuild_RandReadFailure(t *testing.T) {
	signer := newFakeSigner()
	b := NewBuilder(signer)
	b.Now = fixedNow
	b.RandRead = func(_ []byte) (int, error) { return 0, errors.New("entropy starved") }

	env, err := b.Build(validSpec(), validProof(), validBalance())
	assert.Nil(t, env)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrRandFailed), "expected ErrRandFailed, got: %v", err)
	assert.Equal(t, 0, signer.callCount(), "Signer.Sign must NOT be called on RandRead failure")
}

func TestBuild_ValidationFailure(t *testing.T) {
	// A proof that passes ProofInput-shape but fails ValidateProofV1
	// (matcher != "rate_limit") should surface ErrValidation. Construct
	// the proof manually since the proof builder rejects this earlier.
	badProof := validProof()
	badProof.StopFailure.Matcher = "server_error"

	b := newTestBuilder(newFakeSigner())
	env, err := b.Build(validSpec(), badProof, validBalance())
	assert.Nil(t, env)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrValidation), "expected ErrValidation, got: %v", err)
}

func TestBuild_DeterministicMarshal(t *testing.T) {
	// Same inputs (same spec, same proof, same balance, same fixed Now,
	// same zeroRand sequence, same fake-signer canned bytes) → byte-identical
	// DeterministicMarshal of the body across two Build() calls.
	signer := newFakeSigner()
	b := newTestBuilder(signer)

	a, err := b.Build(validSpec(), validProof(), validBalance())
	require.NoError(t, err)
	c, err := b.Build(validSpec(), validProof(), validBalance())
	require.NoError(t, err)

	first, err := proto.MarshalOptions{Deterministic: true}.Marshal(a.Body)
	require.NoError(t, err)
	second, err := proto.MarshalOptions{Deterministic: true}.Marshal(c.Body)
	require.NoError(t, err)
	assert.Equal(t, first, second,
		"identical inputs must produce identical DeterministicMarshal output")
}

func TestBuild_Concurrent(t *testing.T) {
	// 50 goroutines call Build() on the same Builder. The fake signer is
	// concurrency-safe via its mu; Builder itself holds no mutable per-request
	// state. -race must be clean.
	signer := newFakeSigner()
	b := newTestBuilder(signer)

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	results := make([]*tbproto.EnvelopeSigned, N)
	errs := make([]error, N)

	for i := range N {
		go func(i int) {
			defer wg.Done()
			// Distinct BodyHash per goroutine — distinct envelopes.
			spec := validSpec()
			spec.BodyHash = bytesOfLen(32, byte(i+1))
			env, err := b.Build(spec, validProof(), validBalance())
			results[i] = env
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i := range N {
		require.NoErrorf(t, errs[i], "goroutine %d errored", i)
		require.NotNil(t, results[i])
		assert.Equal(t, byte(i+1), results[i].Body.BodyHash[0],
			"goroutine %d produced an envelope with the wrong BodyHash", i)
	}
	assert.Equal(t, N, signer.callCount(), "Signer.Sign called once per goroutine")
}

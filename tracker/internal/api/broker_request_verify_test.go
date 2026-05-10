package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/token-bay/token-bay/shared/exhaustionproof"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
	"github.com/token-bay/token-bay/tracker/internal/admission"
	"github.com/token-bay/token-bay/tracker/internal/api"
	"github.com/token-bay/token-bay/tracker/internal/broker"
)

// fakeIdentityResolver returns a fixed pubkey for one specific consumer ID.
type fakeIdentityResolver struct {
	id  ids.IdentityID
	pub ed25519.PublicKey
}

func (f *fakeIdentityResolver) PeerPubkey(id ids.IdentityID) (ed25519.PublicKey, bool) {
	if id == f.id {
		return f.pub, true
	}
	return nil, false
}

// repSpyV2 captures both broker_request submissions and proof_fidelity emits.
type repSpyV2 struct {
	brokerCalls []ids.IdentityID
	fidelity    []string
}

func (s *repSpyV2) RecordBrokerRequest(c ids.IdentityID, _ string) error {
	s.brokerCalls = append(s.brokerCalls, c)
	return nil
}

func (s *repSpyV2) RecordProofFidelity(_ ids.IdentityID, level string) error {
	s.fidelity = append(s.fidelity, level)
	return nil
}

// signedEnvelopeFixture builds a properly signed EnvelopeSigned with a
// fresh balance snapshot signed by trackerPriv. now is the time the
// snapshot was issued.
type envelopeFixture struct {
	consumerPriv ed25519.PrivateKey
	consumerPub  ed25519.PublicKey
	consumerID   ids.IdentityID
	trackerPriv  ed25519.PrivateKey
	trackerPub   ed25519.PublicKey
	now          time.Time
}

func newEnvelopeFixture(t *testing.T) *envelopeFixture {
	t.Helper()
	cPub, cPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tPub, tPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &envelopeFixture{
		consumerPriv: cPriv,
		consumerPub:  cPub,
		consumerID:   ids.IdentityID(sha256.Sum256(cPub)),
		trackerPriv:  tPriv,
		trackerPub:   tPub,
		now:          time.Unix(1700000000, 0),
	}
}

// build signs and marshals a fixture envelope. The mutator runs on the
// EnvelopeBody before signing to flip individual fields.
func (f *envelopeFixture) build(t *testing.T, mutate func(*tbproto.EnvelopeBody, *tbproto.SignedBalanceSnapshot)) []byte {
	t.Helper()
	ts := uint64(f.now.Unix())
	balBody := &tbproto.BalanceSnapshotBody{
		IdentityId:   f.consumerID[:],
		Credits:      1000,
		ChainTipHash: bytes.Repeat([]byte{0xCC}, 32),
		ChainTipSeq:  1,
		IssuedAt:     ts,
		ExpiresAt:    ts + 600,
	}
	balSig, err := signing.SignBalanceSnapshot(f.trackerPriv, balBody)
	if err != nil {
		t.Fatal(err)
	}
	bal := &tbproto.SignedBalanceSnapshot{Body: balBody, TrackerSig: balSig}

	body := &tbproto.EnvelopeBody{
		ProtocolVersion: 1,
		ConsumerId:      f.consumerID[:],
		Model:           "claude-3-haiku-20240307",
		Tier:            tbproto.PrivacyTier_PRIVACY_TIER_STANDARD,
		BodyHash:        bytes.Repeat([]byte{0xAA}, 32),
		ExhaustionProof: &exhaustionproof.ExhaustionProofV1{
			StopFailure: &exhaustionproof.StopFailure{Matcher: "rate_limit", At: ts},
			UsageProbe:  &exhaustionproof.UsageProbe{At: ts},
			CapturedAt:  ts,
			Nonce:       bytes.Repeat([]byte{0x01}, 16),
		},
		BalanceProof: bal,
		CapturedAt:   ts,
		Nonce:        bytes.Repeat([]byte{0x02}, 16),
	}
	if mutate != nil {
		mutate(body, bal)
	}
	sig, err := signing.SignEnvelope(f.consumerPriv, body)
	if err != nil {
		t.Fatal(err)
	}
	env := &tbproto.EnvelopeSigned{Body: body, ConsumerSig: sig}
	out, err := proto.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func newRCAt(now time.Time) *api.RequestCtx {
	rc := newRC()
	rc.Now = now
	return rc
}

func newVerifyingRouter(t *testing.T, f *envelopeFixture, repSpy api.ReputationRecorder, brk api.BrokerService, adm api.AdmissionService) *api.Router {
	t.Helper()
	r, err := api.NewRouter(api.Deps{
		Broker:     brk,
		Admission:  adm,
		Reputation: repSpy,
		Identity:   &fakeIdentityResolver{id: f.consumerID, pub: f.consumerPub},
		TrackerPub: f.trackerPub,
		Now:        func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBrokerRequest_Verify_Pass_RecordsFidelityFull(t *testing.T) {
	f := newEnvelopeFixture(t)
	spy := &repSpyV2{}
	svc := &fakeBrokerService{submitResult: &broker.Result{
		Outcome: broker.OutcomeAdmit,
		Admit: &broker.Assignment{
			SeederAddr:       "10.0.0.1:4000",
			SeederPubkey:     bytes.Repeat([]byte{0xBB}, 32),
			ReservationToken: bytes.Repeat([]byte{0xCC}, 16),
		},
	}}
	adm := &fakeAdmissionForBroker{decideResult: admission.Result{Outcome: admission.OutcomeAdmit}}
	r := newVerifyingRouter(t, f, spy, svc, adm)

	resp := r.Dispatch(context.Background(), newRCAt(f.now), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
		Payload: f.build(t, nil),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status=%v error=%+v", resp.Status, resp.Error)
	}
	if len(spy.fidelity) != 1 || spy.fidelity[0] != "full_two_signal" {
		t.Errorf("expected fidelity=[full_two_signal], got %v", spy.fidelity)
	}
}

func TestBrokerRequest_Verify_RejectsBadConsumerSig(t *testing.T) {
	f := newEnvelopeFixture(t)
	spy := &repSpyV2{}
	svc := &fakeBrokerService{}
	adm := &fakeAdmissionForBroker{decideResult: admission.Result{Outcome: admission.OutcomeAdmit}}
	r := newVerifyingRouter(t, f, spy, svc, adm)

	// Build then corrupt the consumer signature.
	payload := f.build(t, nil)
	var env tbproto.EnvelopeSigned
	if err := proto.Unmarshal(payload, &env); err != nil {
		t.Fatal(err)
	}
	env.ConsumerSig[0] ^= 0xFF
	corrupt, _ := proto.Marshal(&env)

	resp := r.Dispatch(context.Background(), newRCAt(f.now), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
		Payload: corrupt,
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_UNAUTHENTICATED {
		t.Fatalf("status=%v error=%+v want UNAUTHENTICATED", resp.Status, resp.Error)
	}
	if len(spy.fidelity) != 0 {
		t.Errorf("expected no fidelity emit on sig fail, got %v", spy.fidelity)
	}
}

func TestBrokerRequest_Verify_RejectsUnknownConsumer(t *testing.T) {
	f := newEnvelopeFixture(t)
	spy := &repSpyV2{}
	svc := &fakeBrokerService{}
	adm := &fakeAdmissionForBroker{decideResult: admission.Result{Outcome: admission.OutcomeAdmit}}
	// Identity resolver returns "not found" for everyone.
	r, err := api.NewRouter(api.Deps{
		Broker:     svc,
		Admission:  adm,
		Reputation: spy,
		Identity:   &fakeIdentityResolver{}, // empty — no match
		TrackerPub: f.trackerPub,
		Now:        func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}

	resp := r.Dispatch(context.Background(), newRCAt(f.now), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
		Payload: f.build(t, nil),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_UNAUTHENTICATED {
		t.Fatalf("status=%v error=%+v want UNAUTHENTICATED", resp.Status, resp.Error)
	}
}

func TestBrokerRequest_Verify_RejectsBadBalanceSig(t *testing.T) {
	f := newEnvelopeFixture(t)
	spy := &repSpyV2{}
	svc := &fakeBrokerService{}
	adm := &fakeAdmissionForBroker{decideResult: admission.Result{Outcome: admission.OutcomeAdmit}}
	r := newVerifyingRouter(t, f, spy, svc, adm)

	payload := f.build(t, func(_ *tbproto.EnvelopeBody, bal *tbproto.SignedBalanceSnapshot) {
		bal.TrackerSig = bytes.Repeat([]byte{0x00}, ed25519.SignatureSize)
	})
	resp := r.Dispatch(context.Background(), newRCAt(f.now), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
		Payload: payload,
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INVALID {
		t.Fatalf("status=%v error=%+v want INVALID", resp.Status, resp.Error)
	}
}

func TestBrokerRequest_Verify_RejectsBalanceSignedByForeignTracker(t *testing.T) {
	f := newEnvelopeFixture(t)
	// Sign the balance with a *different* tracker key — server only trusts its own.
	otherPub, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	_ = otherPub

	spy := &repSpyV2{}
	svc := &fakeBrokerService{}
	adm := &fakeAdmissionForBroker{decideResult: admission.Result{Outcome: admission.OutcomeAdmit}}
	r := newVerifyingRouter(t, f, spy, svc, adm)

	ts := uint64(f.now.Unix())
	balBody := &tbproto.BalanceSnapshotBody{
		IdentityId:   f.consumerID[:],
		Credits:      1000,
		ChainTipHash: bytes.Repeat([]byte{0xCC}, 32),
		ChainTipSeq:  1,
		IssuedAt:     ts,
		ExpiresAt:    ts + 600,
	}
	balSig, err := signing.SignBalanceSnapshot(otherPriv, balBody)
	if err != nil {
		t.Fatal(err)
	}
	payload := f.build(t, func(_ *tbproto.EnvelopeBody, bal *tbproto.SignedBalanceSnapshot) {
		bal.Body = balBody
		bal.TrackerSig = balSig
	})

	resp := r.Dispatch(context.Background(), newRCAt(f.now), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
		Payload: payload,
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INVALID {
		t.Fatalf("status=%v error=%+v want INVALID", resp.Status, resp.Error)
	}
}

func TestBrokerRequest_Verify_RejectsStaleBalance(t *testing.T) {
	f := newEnvelopeFixture(t)
	spy := &repSpyV2{}
	svc := &fakeBrokerService{}
	adm := &fakeAdmissionForBroker{decideResult: admission.Result{Outcome: admission.OutcomeAdmit}}
	r := newVerifyingRouter(t, f, spy, svc, adm)

	// Snapshot issued at f.now; dispatch happens 11 minutes later.
	dispatchAt := f.now.Add(11 * time.Minute)
	resp := r.Dispatch(context.Background(), newRCAt(dispatchAt), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
		Payload: f.build(t, nil),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INVALID {
		t.Fatalf("status=%v error=%+v want INVALID", resp.Status, resp.Error)
	}
}

func TestBrokerRequest_Verify_AcceptsBalanceAtTTLBoundary(t *testing.T) {
	f := newEnvelopeFixture(t)
	spy := &repSpyV2{}
	svc := &fakeBrokerService{submitResult: &broker.Result{
		Outcome: broker.OutcomeAdmit,
		Admit: &broker.Assignment{
			SeederAddr:       "10.0.0.1:4000",
			SeederPubkey:     bytes.Repeat([]byte{0xBB}, 32),
			ReservationToken: bytes.Repeat([]byte{0xCC}, 16),
		},
	}}
	adm := &fakeAdmissionForBroker{decideResult: admission.Result{Outcome: admission.OutcomeAdmit}}
	r := newVerifyingRouter(t, f, spy, svc, adm)

	dispatchAt := f.now.Add(10 * time.Minute) // exactly at boundary
	resp := r.Dispatch(context.Background(), newRCAt(dispatchAt), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
		Payload: f.build(t, nil),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status=%v error=%+v want OK at TTL boundary", resp.Status, resp.Error)
	}
}

func TestClassifyProofFidelity(t *testing.T) {
	ts := uint64(1700000000)
	full := &exhaustionproof.ExhaustionProofV1{
		StopFailure: &exhaustionproof.StopFailure{Matcher: "rate_limit", At: ts},
		UsageProbe:  &exhaustionproof.UsageProbe{At: ts},
		CapturedAt:  ts,
		Nonce:       bytes.Repeat([]byte{0x01}, 16),
	}
	missingProbe := &exhaustionproof.ExhaustionProofV1{
		StopFailure: &exhaustionproof.StopFailure{Matcher: "rate_limit", At: ts},
		UsageProbe:  nil,
		CapturedAt:  ts,
		Nonce:       bytes.Repeat([]byte{0x01}, 16),
	}
	missingStop := &exhaustionproof.ExhaustionProofV1{
		StopFailure: nil,
		UsageProbe:  &exhaustionproof.UsageProbe{At: ts},
		CapturedAt:  ts,
		Nonce:       bytes.Repeat([]byte{0x01}, 16),
	}
	wrongMatcher := &exhaustionproof.ExhaustionProofV1{
		StopFailure: &exhaustionproof.StopFailure{Matcher: "synthetic", At: ts},
		UsageProbe:  &exhaustionproof.UsageProbe{At: ts},
		CapturedAt:  ts,
		Nonce:       bytes.Repeat([]byte{0x01}, 16),
	}

	cases := []struct {
		name string
		in   *exhaustionproof.ExhaustionProofV1
		want string
	}{
		{"full_two_signal", full, "full_two_signal"},
		{"partial_missing_probe", missingProbe, "partial"},
		{"partial_missing_stop", missingStop, "partial"},
		{"degraded_wrong_matcher", wrongMatcher, "degraded"},
		{"degraded_nil", nil, "degraded"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := api.ClassifyProofFidelity(c.in)
			if got != c.want {
				t.Errorf("ClassifyProofFidelity = %q, want %q", got, c.want)
			}
		})
	}
}

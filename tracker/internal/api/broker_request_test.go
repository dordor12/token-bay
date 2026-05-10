package api_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	sharedadmission "github.com/token-bay/token-bay/shared/admission"
	"github.com/token-bay/token-bay/shared/exhaustionproof"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/admission"
	"github.com/token-bay/token-bay/tracker/internal/api"
	"github.com/token-bay/token-bay/tracker/internal/broker"
)

// fakeBrokerService satisfies api.BrokerService.
type fakeBrokerService struct {
	submitResult *broker.Result
	submitErr    error
}

func (f *fakeBrokerService) Submit(_ context.Context, _ *tbproto.EnvelopeSigned) (*broker.Result, error) {
	return f.submitResult, f.submitErr
}

func (f *fakeBrokerService) RegisterQueued(_ *tbproto.EnvelopeSigned, _ [16]byte, _ func(*broker.Result)) {
}

// fakeAdmissionForBroker satisfies api.AdmissionService (enrollAdmission +
// brokerAdmission). The Admit method is a passthrough no-op.
type fakeAdmissionForBroker struct {
	decideResult admission.Result
}

func (f *fakeAdmissionForBroker) Admit(_ context.Context, _, _ []byte) error { return nil }
func (f *fakeAdmissionForBroker) Decide(_ ids.IdentityID, _ *sharedadmission.SignedCreditAttestation, _ time.Time) admission.Result {
	return f.decideResult
}

// validEnvelopeBytes returns a marshalled EnvelopeSigned that passes
// ValidateEnvelopeBody. The consumer_id is the supplied id (32 bytes).
func validEnvelopeBytes(t *testing.T, consumerID []byte) []byte {
	t.Helper()
	ts := uint64(1700000000)
	body := &tbproto.EnvelopeBody{
		ProtocolVersion: 1,
		ConsumerId:      consumerID,
		Model:           "claude-3-haiku-20240307",
		Tier:            tbproto.PrivacyTier_PRIVACY_TIER_STANDARD,
		BodyHash:        bytes.Repeat([]byte{0xAA}, 32),
		ExhaustionProof: &exhaustionproof.ExhaustionProofV1{
			StopFailure: &exhaustionproof.StopFailure{Matcher: "rate_limit", At: ts},
			UsageProbe:  &exhaustionproof.UsageProbe{At: ts},
			CapturedAt:  ts,
			Nonce:       bytes.Repeat([]byte{0x01}, 16),
		},
		BalanceProof: &tbproto.SignedBalanceSnapshot{},
		CapturedAt:   ts,
		Nonce:        bytes.Repeat([]byte{0x02}, 16),
	}
	env := &tbproto.EnvelopeSigned{Body: body}
	b, err := proto.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func consumerID32() []byte { return bytes.Repeat([]byte{0x11}, 32) }

func TestBrokerRequest_Admit_ReturnsSeederAssignment(t *testing.T) {
	svc := &fakeBrokerService{
		submitResult: &broker.Result{
			Outcome: broker.OutcomeAdmit,
			Admit: &broker.Assignment{
				SeederAddr:       "10.0.0.1:4000",
				SeederPubkey:     bytes.Repeat([]byte{0xBB}, 32),
				ReservationToken: bytes.Repeat([]byte{0xCC}, 16),
			},
		},
	}
	adm := &fakeAdmissionForBroker{decideResult: admission.Result{Outcome: admission.OutcomeAdmit}}
	r, _ := api.NewRouter(api.Deps{Broker: svc, Admission: adm})

	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
		Payload: validEnvelopeBytes(t, consumerID32()),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status=%v error=%+v", resp.Status, resp.Error)
	}
	var brr tbproto.BrokerRequestResponse
	if err := proto.Unmarshal(resp.Payload, &brr); err != nil {
		t.Fatal(err)
	}
	sa, ok := brr.Outcome.(*tbproto.BrokerRequestResponse_SeederAssignment)
	if !ok {
		t.Fatalf("expected SeederAssignment, got %T", brr.Outcome)
	}
	if string(sa.SeederAssignment.SeederAddr) != "10.0.0.1:4000" {
		t.Errorf("seeder_addr = %q", sa.SeederAssignment.SeederAddr)
	}
}

func TestBrokerRequest_Queued_ReturnsQueued(t *testing.T) {
	var reqID [16]byte
	reqID[0] = 0x42
	adm := &fakeAdmissionForBroker{
		decideResult: admission.Result{
			Outcome: admission.OutcomeQueue,
			Queued: &admission.QueuedDetails{
				RequestID:    reqID,
				PositionBand: admission.PositionBand1To10,
				EtaBand:      admission.EtaBandLessThan30s,
			},
		},
	}
	r, _ := api.NewRouter(api.Deps{Broker: &fakeBrokerService{}, Admission: adm})

	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
		Payload: validEnvelopeBytes(t, consumerID32()),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status=%v error=%+v", resp.Status, resp.Error)
	}
	var brr tbproto.BrokerRequestResponse
	if err := proto.Unmarshal(resp.Payload, &brr); err != nil {
		t.Fatal(err)
	}
	q, ok := brr.Outcome.(*tbproto.BrokerRequestResponse_Queued)
	if !ok {
		t.Fatalf("expected Queued, got %T", brr.Outcome)
	}
	if len(q.Queued.RequestId) != 16 || q.Queued.RequestId[0] != 0x42 {
		t.Errorf("unexpected request_id: %v", q.Queued.RequestId)
	}
	if q.Queued.PositionBand != tbproto.PositionBand_POSITION_BAND_1_TO_10 {
		t.Errorf("position_band = %v", q.Queued.PositionBand)
	}
}

func TestBrokerRequest_Rejected_ReturnsRejected(t *testing.T) {
	adm := &fakeAdmissionForBroker{
		decideResult: admission.Result{
			Outcome: admission.OutcomeReject,
			Rejected: &admission.RejectedDetails{
				Reason:      admission.RejectReasonRegionOverloaded,
				RetryAfterS: 60,
			},
		},
	}
	r, _ := api.NewRouter(api.Deps{Broker: &fakeBrokerService{}, Admission: adm})

	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
		Payload: validEnvelopeBytes(t, consumerID32()),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status=%v error=%+v", resp.Status, resp.Error)
	}
	var brr tbproto.BrokerRequestResponse
	if err := proto.Unmarshal(resp.Payload, &brr); err != nil {
		t.Fatal(err)
	}
	rej, ok := brr.Outcome.(*tbproto.BrokerRequestResponse_Rejected)
	if !ok {
		t.Fatalf("expected Rejected, got %T", brr.Outcome)
	}
	if rej.Rejected.Reason != tbproto.RejectReason_REJECT_REASON_REGION_OVERLOADED {
		t.Errorf("reason = %v", rej.Rejected.Reason)
	}
	if rej.Rejected.RetryAfterS != 60 {
		t.Errorf("retry_after_s = %d", rej.Rejected.RetryAfterS)
	}
}

func TestBrokerRequest_NoCapacity_FromBroker(t *testing.T) {
	svc := &fakeBrokerService{
		submitResult: &broker.Result{
			Outcome: broker.OutcomeNoCapacity,
			NoCap:   &broker.NoCapacityDetails{Reason: "no_eligible_seeder"},
		},
	}
	adm := &fakeAdmissionForBroker{decideResult: admission.Result{Outcome: admission.OutcomeAdmit}}
	r, _ := api.NewRouter(api.Deps{Broker: svc, Admission: adm})

	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
		Payload: validEnvelopeBytes(t, consumerID32()),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status=%v error=%+v", resp.Status, resp.Error)
	}
	var brr tbproto.BrokerRequestResponse
	if err := proto.Unmarshal(resp.Payload, &brr); err != nil {
		t.Fatal(err)
	}
	nc, ok := brr.Outcome.(*tbproto.BrokerRequestResponse_NoCapacity)
	if !ok {
		t.Fatalf("expected NoCapacity, got %T", brr.Outcome)
	}
	if nc.NoCapacity.Reason != "no_eligible_seeder" {
		t.Errorf("reason = %q", nc.NoCapacity.Reason)
	}
}

func TestBrokerRequest_AdmissionDeps_NotImpl(t *testing.T) {
	// Admission == nil → notImpl stub.
	r, _ := api.NewRouter(api.Deps{Broker: &fakeBrokerService{}})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
	})
	if resp.Error == nil || resp.Error.Code != "NOT_IMPLEMENTED" {
		t.Fatalf("got %+v", resp.Error)
	}
}

func TestBrokerRequest_NilBroker_NotImplemented(t *testing.T) {
	r, _ := api.NewRouter(api.Deps{})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
	})
	if resp.Error == nil || resp.Error.Code != "NOT_IMPLEMENTED" {
		t.Fatalf("got %+v", resp.Error)
	}
}

// repSpy is a test double for api.ReputationRecorder.
type repSpy struct {
	calls []ids.IdentityID
}

func (s *repSpy) RecordBrokerRequest(c ids.IdentityID, _ string) error {
	s.calls = append(s.calls, c)
	return nil
}

func (s *repSpy) RecordProofFidelity(_ ids.IdentityID, _ string) error { return nil }

func TestBrokerRequest_RecordsReputationSignal(t *testing.T) {
	spy := &repSpy{}
	svc := &fakeBrokerService{
		submitResult: &broker.Result{
			Outcome: broker.OutcomeAdmit,
			Admit: &broker.Assignment{
				SeederAddr:       "10.0.0.1:4000",
				SeederPubkey:     bytes.Repeat([]byte{0xBB}, 32),
				ReservationToken: bytes.Repeat([]byte{0xCC}, 16),
			},
		},
	}
	adm := &fakeAdmissionForBroker{decideResult: admission.Result{Outcome: admission.OutcomeAdmit}}
	r, _ := api.NewRouter(api.Deps{Broker: svc, Admission: adm, Reputation: spy})

	consumerBytes := consumerID32()
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
		Payload: validEnvelopeBytes(t, consumerBytes),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status=%v error=%+v", resp.Status, resp.Error)
	}
	if len(spy.calls) != 1 {
		t.Fatalf("expected 1 reputation call, got %d", len(spy.calls))
	}
	var wantID ids.IdentityID
	copy(wantID[:], consumerBytes)
	if spy.calls[0] != wantID {
		t.Errorf("recorded consumer ID = %v, want %v", spy.calls[0], wantID)
	}
}

func TestBrokerRequest_NilReputation_DoesNotPanic(t *testing.T) {
	// Reputation == nil should be silently skipped — no panic.
	svc := &fakeBrokerService{
		submitResult: &broker.Result{
			Outcome: broker.OutcomeAdmit,
			Admit: &broker.Assignment{
				SeederAddr:       "10.0.0.1:4000",
				SeederPubkey:     bytes.Repeat([]byte{0xBB}, 32),
				ReservationToken: bytes.Repeat([]byte{0xCC}, 16),
			},
		},
	}
	adm := &fakeAdmissionForBroker{decideResult: admission.Result{Outcome: admission.OutcomeAdmit}}
	r, _ := api.NewRouter(api.Deps{Broker: svc, Admission: adm}) // no Reputation

	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
		Payload: validEnvelopeBytes(t, consumerID32()),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status=%v error=%+v", resp.Status, resp.Error)
	}
}

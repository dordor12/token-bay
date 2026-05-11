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
//
// When queuedDeliver is non-nil, RegisterQueued spawns a goroutine that
// invokes deliver with queuedDeliver after queuedDelay — letting tests
// drive the api-layer block-then-deliver path with deterministic outcomes.
// queuedCancels counts CancelQueued calls so tests can assert cleanup;
// it is written only by the handler goroutine that called Dispatch and
// read after Dispatch returns, so no synchronization is needed.
type fakeBrokerService struct {
	submitResult  *broker.Result
	submitErr     error
	queuedDeliver *broker.Result
	queuedDelay   time.Duration
	queuedCancels int
}

func (f *fakeBrokerService) Submit(_ context.Context, _ *tbproto.EnvelopeSigned) (*broker.Result, error) {
	return f.submitResult, f.submitErr
}

func (f *fakeBrokerService) RegisterQueued(_ *tbproto.EnvelopeSigned, _ [16]byte, deliver func(*broker.Result)) {
	if f.queuedDeliver == nil {
		return
	}
	res := f.queuedDeliver
	delay := f.queuedDelay
	go func() {
		if delay > 0 {
			time.Sleep(delay)
		}
		deliver(res)
	}()
}

func (f *fakeBrokerService) CancelQueued(_ [16]byte) { f.queuedCancels++ }

// fakeAdmissionForBroker satisfies api.AdmissionService (enrollAdmission +
// brokerAdmission). The Admit method is a passthrough no-op.
type fakeAdmissionForBroker struct {
	decideResult admission.Result
	queueTimeout time.Duration
}

func (f *fakeAdmissionForBroker) Admit(_ context.Context, _, _ []byte) error { return nil }
func (f *fakeAdmissionForBroker) Decide(_ ids.IdentityID, _ *sharedadmission.SignedCreditAttestation, _ time.Time) admission.Result {
	return f.decideResult
}
func (f *fakeAdmissionForBroker) QueueTimeout() time.Duration { return f.queueTimeout }

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

// queuedAdmissionResult returns a Result with an OutcomeQueue decision and
// the test request_id 0x42… so per-test setup stays terse.
func queuedAdmissionResult() admission.Result {
	var reqID [16]byte
	reqID[0] = 0x42
	return admission.Result{
		Outcome: admission.OutcomeQueue,
		Queued: &admission.QueuedDetails{
			RequestID:    reqID,
			PositionBand: admission.PositionBand1To10,
			EtaBand:      admission.EtaBandLessThan30s,
		},
	}
}

// When admission queues an envelope, the handler must block on the broker's
// deliver callback and return the eventual outcome — not the wire Queued
// variant — so a queued consumer ultimately gets served (tracker-broker-design
// §5.4). This test exercises the happy path: drain delivers OutcomeAdmit, the
// RPC response carries the SeederAssignment.
func TestBrokerRequest_Queued_DeliversAdmit(t *testing.T) {
	svc := &fakeBrokerService{
		queuedDeliver: &broker.Result{
			Outcome: broker.OutcomeAdmit,
			Admit: &broker.Assignment{
				SeederAddr:       "10.0.0.5:4000",
				SeederPubkey:     bytes.Repeat([]byte{0xDD}, 32),
				ReservationToken: bytes.Repeat([]byte{0xEE}, 16),
			},
		},
		queuedDelay: 5 * time.Millisecond,
	}
	adm := &fakeAdmissionForBroker{decideResult: queuedAdmissionResult()}
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
	if string(sa.SeederAssignment.SeederAddr) != "10.0.0.5:4000" {
		t.Errorf("seeder_addr = %q", sa.SeederAssignment.SeederAddr)
	}
	if svc.queuedCancels != 0 {
		t.Errorf("unexpected CancelQueued calls on happy path: %d", svc.queuedCancels)
	}
}

// Drain may also deliver NoCapacity for a queued envelope (all seeders fell
// off between queueing and pop). The handler must translate that into the
// wire NoCapacity variant rather than wedging on the channel.
func TestBrokerRequest_Queued_DeliversNoCapacity(t *testing.T) {
	svc := &fakeBrokerService{
		queuedDeliver: &broker.Result{
			Outcome: broker.OutcomeNoCapacity,
			NoCap:   &broker.NoCapacityDetails{Reason: "no_eligible_seeder"},
		},
	}
	adm := &fakeAdmissionForBroker{decideResult: queuedAdmissionResult()}
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

// When the admission queue timeout elapses before drain delivers, the
// handler must emit a wire Rejected{QUEUE_TIMEOUT} and CancelQueued the
// pending entry so a later drain pop doesn't burn a reservation on the
// abandoned envelope.
func TestBrokerRequest_Queued_TimesOut(t *testing.T) {
	svc := &fakeBrokerService{} // no queuedDeliver → handler waits for timeout
	adm := &fakeAdmissionForBroker{
		decideResult: queuedAdmissionResult(),
		queueTimeout: 30 * time.Millisecond,
	}
	r, _ := api.NewRouter(api.Deps{Broker: svc, Admission: adm})

	start := time.Now()
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
		Payload: validEnvelopeBytes(t, consumerID32()),
	})
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("returned too quickly (%v) — should have blocked until queue timeout", elapsed)
	}
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
	if rej.Rejected.Reason != tbproto.RejectReason_REJECT_REASON_QUEUE_TIMEOUT {
		t.Errorf("reason = %v want QUEUE_TIMEOUT", rej.Rejected.Reason)
	}
	if svc.queuedCancels != 1 {
		t.Errorf("CancelQueued calls = %d, want 1", svc.queuedCancels)
	}
}

// A canceled RPC context must release the block-then-deliver wait promptly
// and CancelQueued the pending entry.
func TestBrokerRequest_Queued_CtxCanceled(t *testing.T) {
	svc := &fakeBrokerService{} // never delivers
	adm := &fakeAdmissionForBroker{
		decideResult: queuedAdmissionResult(),
		queueTimeout: time.Hour, // ensure timer never wins
	}
	r, _ := api.NewRouter(api.Deps{Broker: svc, Admission: adm})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	resp := r.Dispatch(ctx, newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
		Payload: validEnvelopeBytes(t, consumerID32()),
	})
	// ctx.Err() bubbles up as an INTERNAL error response (the transport
	// has already gone away; the wire response is moot but should not be OK).
	if resp.Status == tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("expected non-OK status on ctx cancellation, got OK")
	}
	if svc.queuedCancels != 1 {
		t.Errorf("CancelQueued calls = %d, want 1", svc.queuedCancels)
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

func TestBrokerRequest_BrokerErrIdentityFrozen_MapsToFROZEN(t *testing.T) {
	svc := &fakeBrokerService{submitErr: broker.ErrIdentityFrozen}
	adm := &fakeAdmissionForBroker{decideResult: admission.Result{Outcome: admission.OutcomeAdmit}}
	r, _ := api.NewRouter(api.Deps{Broker: svc, Admission: adm})

	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method:  tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST,
		Payload: validEnvelopeBytes(t, consumerID32()),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_FROZEN {
		t.Fatalf("status=%v error=%+v want FROZEN", resp.Status, resp.Error)
	}
	if resp.Error == nil || resp.Error.Code != "FROZEN" {
		t.Fatalf("err=%+v want code=FROZEN", resp.Error)
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

package api

import (
	"context"
	"errors"
	"time"

	"google.golang.org/protobuf/proto"

	sharedadmission "github.com/token-bay/token-bay/shared/admission"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/admission"
	"github.com/token-bay/token-bay/tracker/internal/broker"
)

// brokerAdmission is the slice of admission the broker_request handler needs.
type brokerAdmission interface {
	Decide(consumerID ids.IdentityID, att *sharedadmission.SignedCreditAttestation, now time.Time) admission.Result
}

// installBrokerRequest wires the live broker_request handler when both
// Deps.Broker and Deps.Admission are non-nil; otherwise returns the
// ErrNotImplemented stub.
func (r *Router) installBrokerRequest() handlerFunc {
	if r.deps.Broker == nil || r.deps.Admission == nil {
		return notImpl("broker_request")
	}
	adm, ok := r.deps.Admission.(brokerAdmission)
	if !ok {
		return notImpl("broker_request")
	}
	return func(ctx context.Context, rc *RequestCtx, payloadBytes []byte) (*tbproto.RpcResponse, error) {
		var env tbproto.EnvelopeSigned
		if err := proto.Unmarshal(payloadBytes, &env); err != nil {
			return nil, ErrInvalid("EnvelopeSigned: " + err.Error())
		}
		if env.Body == nil {
			return nil, ErrInvalid("EnvelopeSigned.Body nil")
		}
		if err := tbproto.ValidateEnvelopeBody(env.Body); err != nil {
			return nil, ErrInvalid("EnvelopeBody: " + err.Error())
		}

		var consumer ids.IdentityID
		copy(consumer[:], env.Body.ConsumerId)

		if r.deps.Reputation != nil {
			_ = r.deps.Reputation.RecordBrokerRequest(consumer, "submitted")
		}

		// TODO(broker-followup): verify consumer sig + balance proof + exhaustion proof.
		// v1 trusts the mTLS-authenticated channel + admission gating.

		admResult := adm.Decide(consumer, nil, rc.Now)
		switch admResult.Outcome {
		case admission.OutcomeAdmit:
			res, err := r.deps.Broker.Submit(ctx, &env)
			if err != nil {
				if errors.Is(err, broker.ErrUnknownModel) {
					return nil, ErrInvalid("UNKNOWN_MODEL")
				}
				return nil, err
			}
			return brokerResultToResponse(res)

		case admission.OutcomeQueue:
			// v1: emit Queued wire response without blocking the RPC.
			// TODO(broker-followup): wire RegisterQueued + block-then-deliver path.
			return queuedToResponse(admResult.Queued)

		case admission.OutcomeReject:
			return rejectedToResponse(admResult.Rejected)

		default:
			return nil, errors.New("api: admission returned unknown outcome")
		}
	}
}

func brokerResultToResponse(res *broker.Result) (*tbproto.RpcResponse, error) {
	var brr tbproto.BrokerRequestResponse
	switch res.Outcome {
	case broker.OutcomeAdmit:
		brr.Outcome = &tbproto.BrokerRequestResponse_SeederAssignment{
			SeederAssignment: &tbproto.SeederAssignment{
				SeederAddr:       []byte(res.Admit.SeederAddr),
				SeederPubkey:     res.Admit.SeederPubkey,
				ReservationToken: res.Admit.ReservationToken,
			},
		}
	case broker.OutcomeNoCapacity:
		brr.Outcome = &tbproto.BrokerRequestResponse_NoCapacity{
			NoCapacity: &tbproto.NoCapacity{Reason: res.NoCap.Reason},
		}
	default:
		return nil, errors.New("api: unexpected broker outcome")
	}
	out, err := proto.Marshal(&brr)
	if err != nil {
		return nil, err
	}
	return OkResponse(out), nil
}

func queuedToResponse(q *admission.QueuedDetails) (*tbproto.RpcResponse, error) {
	var brr tbproto.BrokerRequestResponse
	brr.Outcome = &tbproto.BrokerRequestResponse_Queued{
		Queued: &tbproto.Queued{
			RequestId:    q.RequestID[:],
			PositionBand: tbproto.PositionBand(q.PositionBand),
			EtaBand:      tbproto.EtaBand(q.EtaBand),
		},
	}
	out, err := proto.Marshal(&brr)
	if err != nil {
		return nil, err
	}
	return OkResponse(out), nil
}

func rejectedToResponse(rd *admission.RejectedDetails) (*tbproto.RpcResponse, error) {
	var brr tbproto.BrokerRequestResponse
	brr.Outcome = &tbproto.BrokerRequestResponse_Rejected{
		Rejected: &tbproto.Rejected{
			Reason:      tbproto.RejectReason(rd.Reason),
			RetryAfterS: rd.RetryAfterS,
		},
	}
	out, err := proto.Marshal(&brr)
	if err != nil {
		return nil, err
	}
	return OkResponse(out), nil
}

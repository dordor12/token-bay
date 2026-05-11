package api

import (
	"context"
	"crypto/ed25519"
	"errors"
	"time"

	"google.golang.org/protobuf/proto"

	sharedadmission "github.com/token-bay/token-bay/shared/admission"
	"github.com/token-bay/token-bay/shared/exhaustionproof"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
	"github.com/token-bay/token-bay/tracker/internal/admission"
	"github.com/token-bay/token-bay/tracker/internal/broker"
)

// balanceSnapshotTTL is the maximum age of a SignedBalanceSnapshot the
// broker_request handler will accept. Mirrors tracker spec §6 ("signed
// balance snapshots have a 10-minute TTL").
const balanceSnapshotTTL = 10 * time.Minute

// ClassifyProofFidelity bucketises an ExhaustionProofV1 into the
// proof_fidelity_level categories from reputation spec §3.1:
//   - "full_two_signal" — both stop_failure and usage_probe present and
//     self-consistent (matcher == "rate_limit").
//   - "partial" — exactly one of the two signals missing.
//   - "degraded" — both signals missing, the proof itself is nil, or the
//     stop_failure carries a non-rate-limit matcher (text/synthetic).
//
// The handler emits a per-broker_request signal so the evaluator can
// detect consumers whose proofs systematically degrade in quality.
func ClassifyProofFidelity(p *exhaustionproof.ExhaustionProofV1) string {
	if p == nil {
		return "degraded"
	}
	hasStop := p.StopFailure != nil
	hasProbe := p.UsageProbe != nil
	switch {
	case !hasStop && !hasProbe:
		return "degraded"
	case !hasStop || !hasProbe:
		return "partial"
	}
	if p.StopFailure.Matcher != "rate_limit" {
		return "degraded"
	}
	return "full_two_signal"
}

// brokerAdmission is the slice of admission the broker_request handler needs.
type brokerAdmission interface {
	Decide(consumerID ids.IdentityID, att *sharedadmission.SignedCreditAttestation, now time.Time) admission.Result
	// QueueTimeout caps the wait of the block-then-deliver path for an
	// OutcomeQueue decision. Zero means "no operator-configured cap" —
	// the handler falls back to a safety bound (queueTimeoutFallback).
	QueueTimeout() time.Duration
}

// queueTimeoutFallback bounds the block-then-deliver wait when admission
// reports zero (boot-time / mis-configured). Mirrors the 300 s admission-design
// default, surfaced here so a wedged admission can never park an RPC forever.
const queueTimeoutFallback = 300 * time.Second

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
	verifyEnabled := r.deps.Identity != nil && len(r.deps.TrackerPub) == ed25519.PublicKeySize
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

		if verifyEnabled {
			if err := r.verifyBrokerRequest(&env, consumer, rc.Now); err != nil {
				return nil, err
			}
			fidelity := ClassifyProofFidelity(env.Body.ExhaustionProof)
			if r.deps.Reputation != nil {
				_ = r.deps.Reputation.RecordProofFidelity(consumer, fidelity)
			}
		}

		admResult := adm.Decide(consumer, nil, rc.Now)
		switch admResult.Outcome {
		case admission.OutcomeAdmit:
			res, err := r.deps.Broker.Submit(ctx, &env)
			if err != nil {
				if errors.Is(err, broker.ErrUnknownModel) {
					return nil, ErrInvalid("UNKNOWN_MODEL")
				}
				if errors.Is(err, broker.ErrIdentityFrozen) {
					return nil, ErrFrozen("identity revoked by peer tracker")
				}
				return nil, err
			}
			return brokerResultToResponse(res)

		case admission.OutcomeQueue:
			if admResult.Queued == nil {
				return nil, errors.New("api: admission returned OutcomeQueue with nil Queued")
			}
			return r.serveQueued(ctx, &env, adm, admResult.Queued)

		case admission.OutcomeReject:
			return rejectedToResponse(admResult.Rejected)

		default:
			return nil, errors.New("api: admission returned unknown outcome")
		}
	}
}

// serveQueued implements the block-then-deliver path (tracker-broker-design
// §5.4): register the envelope with the broker's pendingQueued cache, then
// park on a 1-buffer channel until (a) the queue-drain goroutine pops the
// entry and Submit's eventual result is delivered, (b) the consumer's RPC
// ctx is canceled, or (c) the admission queue timeout elapses. On (b)/(c)
// the pendingQueued entry is CancelQueued to keep drain from later burning
// a reservation on behalf of a consumer that has already given up.
func (r *Router) serveQueued(
	ctx context.Context,
	env *tbproto.EnvelopeSigned,
	adm brokerAdmission,
	q *admission.QueuedDetails,
) (*tbproto.RpcResponse, error) {
	requestID := q.RequestID
	ch := make(chan *broker.Result, 1)
	r.deps.Broker.RegisterQueued(env, requestID, func(res *broker.Result) {
		select {
		case ch <- res:
		default:
		}
	})

	timeout := adm.QueueTimeout()
	if timeout <= 0 {
		timeout = queueTimeoutFallback
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case res := <-ch:
		if res == nil {
			return nil, errors.New("api: broker delivered nil result for queued envelope")
		}
		return brokerResultToResponse(res)
	case <-timer.C:
		r.deps.Broker.CancelQueued(requestID)
		retry := uint32(timeout / time.Second) //nolint:gosec // G115 — timeout is bounded
		return rejectedToResponse(&admission.RejectedDetails{
			Reason:      admission.RejectReasonQueueTimeout,
			RetryAfterS: retry,
		})
	case <-ctx.Done():
		r.deps.Broker.CancelQueued(requestID)
		return nil, ctx.Err()
	}
}

// verifyBrokerRequest enforces the three v1 cryptographic invariants on
// a broker_request envelope:
//
//  1. The consumer signature on the envelope verifies under the pubkey
//     resolved from the live mTLS connection table for ConsumerId. A
//     consumer not currently connected, or a sig that does not check
//     out, returns ErrUnauthenticated.
//  2. The embedded SignedBalanceSnapshot verifies under the local
//     tracker pubkey and is no older than balanceSnapshotTTL. A foreign
//     issuer or stale snapshot returns ErrInvalid.
//  3. The ExhaustionProofV1 has already passed wire-format validation
//     in ValidateEnvelopeBody; the handler classifies its fidelity and
//     emits the proof_fidelity_level signal.
//
// Per reputation §3.1, the classifier output is a per-broker_request
// signal; the evaluator detects consumers whose proofs degrade
// systematically over a population baseline.
func (r *Router) verifyBrokerRequest(env *tbproto.EnvelopeSigned, consumer ids.IdentityID, now time.Time) error {
	pub, ok := r.deps.Identity.PeerPubkey(consumer)
	if !ok {
		return ErrUnauthenticated("consumer not connected")
	}
	if !signing.VerifyEnvelope(pub, env) {
		return ErrUnauthenticated("envelope signature invalid")
	}

	bal := env.Body.BalanceProof
	if bal == nil || bal.Body == nil {
		return ErrInvalid("balance_proof missing body")
	}
	if !signing.VerifyBalanceSnapshot(r.deps.TrackerPub, bal) {
		return ErrInvalid("balance_proof signature invalid")
	}
	issued := time.Unix(int64(bal.Body.IssuedAt), 0) //nolint:gosec // G115 — IssuedAt is unix seconds
	if now.Sub(issued) > balanceSnapshotTTL {
		return ErrInvalid("balance_proof stale")
	}

	// ExhaustionProofV1 wire-format already validated by
	// ValidateEnvelopeBody. v1 has no signature on the proof itself;
	// the classifier downstream is the per-proof signal source.
	return nil
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

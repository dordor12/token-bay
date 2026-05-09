package broker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"sync"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/registry"
	"github.com/token-bay/token-bay/tracker/internal/session"
)

// Broker is the central coordination subsystem for a regional tracker. It
// maps incoming consumer requests to available seeders, manages in-flight
// request state, and enforces credit reservations. See broker-design §3.1
// and §5.1 for the authoritative description of the algorithm.
//
// Construct via OpenBroker; tear down via Close.
type Broker struct {
	cfg  config.BrokerConfig
	scfg config.SettlementConfig
	deps Deps

	mgr *session.Manager

	pendingMu     sync.Mutex
	pendingQueued map[[16]byte]pendingEnv
	queueDrainCh  chan struct{}

	stop chan struct{}
	wg   sync.WaitGroup
}

// pendingEnv is a queued envelope waiting for capacity. Populated by Task 15
// when the admission subsystem returns OutcomeQueued.
type pendingEnv struct {
	body    *tbproto.EnvelopeBody
	deliver chan *Result
}

// OpenBroker constructs a ready Broker from the provided configuration, deps,
// and (optional) pre-allocated session.Manager. Required deps: Registry,
// Ledger, Admission, Pusher, Pricing. Optional: Now (defaults to time.Now),
// Reputation (defaults to fallbackReputation{}). When mgr is nil a fresh
// session.Manager is allocated.
func OpenBroker(cfg config.BrokerConfig, scfg config.SettlementConfig, deps Deps, mgr *session.Manager) (*Broker, error) {
	if deps.Registry == nil {
		return nil, errors.New("broker: Registry required")
	}
	if deps.Ledger == nil {
		return nil, errors.New("broker: Ledger required")
	}
	if deps.Admission == nil {
		return nil, errors.New("broker: Admission required")
	}
	if deps.Pusher == nil {
		return nil, errors.New("broker: Pusher required")
	}
	if deps.Pricing == nil {
		return nil, errors.New("broker: Pricing required")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Reputation == nil {
		deps.Reputation = fallbackReputation{}
	}
	if mgr == nil {
		mgr = session.New()
	}
	b := &Broker{
		cfg:           cfg,
		scfg:          scfg,
		deps:          deps,
		mgr:           mgr,
		pendingQueued: make(map[[16]byte]pendingEnv),
		queueDrainCh:  make(chan struct{}, 1),
		stop:          make(chan struct{}),
	}
	b.startQueueDrain()
	return b, nil
}

// Close shuts down all broker goroutines. Idempotent.
func (b *Broker) Close() error {
	select {
	case <-b.stop:
		return nil
	default:
		close(b.stop)
	}
	b.wg.Wait()
	return nil
}

// Submit implements the broker selection algorithm (§5.1). Pre-condition:
// api/ has fully validated the envelope and admission has returned
// OutcomeAdmit; broker.Submit assumes the envelope is sound.
func (b *Broker) Submit(ctx context.Context, env *tbproto.EnvelopeSigned) (*Result, error) {
	if env == nil || env.Body == nil {
		return nil, errors.New("broker: Submit nil envelope")
	}
	body := env.Body
	var consumer ids.IdentityID
	copy(consumer[:], body.ConsumerId)

	cost, err := b.deps.Pricing.MaxCost(body)
	if err != nil {
		return nil, err
	}

	var requestID [16]byte
	if _, err := rand.Read(requestID[:]); err != nil {
		return nil, err
	}

	bodyBytes, err := signing.DeterministicMarshal(body)
	if err != nil {
		return nil, err
	}
	envHash := sha256.Sum256(bodyBytes)

	now := b.deps.Now()
	expiresAt := now.Add(time.Duration(b.scfg.ReservationTTLS) * time.Second)

	var creds uint64
	if body.BalanceProof != nil && body.BalanceProof.Body != nil {
		if c := body.BalanceProof.Body.Credits; c > 0 {
			creds = uint64(c) //nolint:gosec // G115: guarded by c > 0
		}
	}

	if rerr := b.mgr.Reservations.Reserve(requestID, consumer, cost, creds, expiresAt); rerr != nil {
		if errors.Is(rerr, session.ErrInsufficientCredits) {
			return &Result{
				Outcome: OutcomeNoCapacity,
				NoCap:   &NoCapacityDetails{Reason: "insufficient_credits"},
			}, nil
		}
		return nil, rerr
	}

	req := &session.Request{
		RequestID:       requestID,
		ConsumerID:      consumer,
		EnvelopeBody:    body,
		EnvelopeHash:    envHash,
		MaxCostReserved: cost,
		StartedAt:       now,
		State:           session.StateSelecting,
	}
	b.mgr.Inflight.Insert(req)

	var tried []ids.IdentityID
	triedAny := false
	for attempt := 0; attempt < b.cfg.MaxOfferAttempts; attempt++ {
		snap := b.deps.Registry.Match(registry.Filter{
			RequireAvailable: true,
			Model:            body.Model,
			Tier:             body.Tier,
			MinHeadroom:      b.cfg.HeadroomThreshold,
			MaxLoad:          b.cfg.LoadThreshold,
		})
		cands := Pick(snap, body, b.cfg.ScoreWeights, b.deps.Reputation, b.cfg, tried)
		if len(cands) == 0 {
			break
		}
		seeder := cands[0].Record

		if _, err := b.deps.Registry.IncLoad(seeder.IdentityID); err != nil {
			tried = append(tried, seeder.IdentityID)
			triedAny = true
			continue
		}

		accepted, ephPub, oerr := runOffer(ctx, b.deps.Pusher, seeder.IdentityID, body, envHash,
			time.Duration(b.cfg.OfferTimeoutMs)*time.Millisecond)
		if errors.Is(oerr, context.Canceled) || errors.Is(oerr, context.DeadlineExceeded) {
			_, _ = b.deps.Registry.DecLoad(seeder.IdentityID)
			b.failAndRelease(req)
			return nil, oerr
		}
		if oerr != nil {
			_, _ = b.deps.Registry.DecLoad(seeder.IdentityID)
			b.deps.Reputation.RecordOfferOutcome(seeder.IdentityID, "unreachable")
			tried = append(tried, seeder.IdentityID)
			triedAny = true
			continue
		}
		if !accepted {
			_, _ = b.deps.Registry.DecLoad(seeder.IdentityID)
			b.deps.Reputation.RecordOfferOutcome(seeder.IdentityID, "reject")
			tried = append(tried, seeder.IdentityID)
			triedAny = true
			continue
		}

		b.deps.Reputation.RecordOfferOutcome(seeder.IdentityID, "accept")
		// Accepted — load stays incremented; settlement releases on terminal.
		_ = b.mgr.Inflight.MarkSeeder(req.RequestID, seeder.IdentityID, ephPub)
		if terr := b.mgr.Inflight.Transition(req.RequestID, session.StateSelecting, session.StateAssigned); terr != nil {
			_, _ = b.deps.Registry.DecLoad(seeder.IdentityID)
			b.failAndRelease(req)
			return nil, terr
		}
		return &Result{
			Outcome: OutcomeAdmit,
			Admit: &Assignment{
				SeederAddr:       seeder.NetCoords.ExternalAddr.String(),
				SeederPubkey:     ephPub,
				ReservationToken: append([]byte(nil), requestID[:]...),
				RequestID:        requestID,
				AssignedSeeder:   seeder.IdentityID,
			},
		}, nil
	}

	b.failAndRelease(req)
	reason := "no_eligible_seeder"
	if triedAny {
		reason = "all_seeders_rejected"
	}
	return &Result{
		Outcome: OutcomeNoCapacity,
		NoCap:   &NoCapacityDetails{Reason: reason},
	}, nil
}

// failAndRelease releases the reservation and marks the request as failed.
func (b *Broker) failAndRelease(req *session.Request) {
	_, _, _ = b.mgr.Reservations.Release(req.RequestID)
	_ = b.mgr.Inflight.Transition(req.RequestID, session.StateSelecting, session.StateFailed)
}

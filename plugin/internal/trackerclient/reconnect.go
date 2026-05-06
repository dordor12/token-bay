package trackerclient

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient/internal/transport"
)

// backoffDelay returns the next backoff value for the supervisor. The
// returned duration is bounded by [BackoffBase, BackoffMax] and includes
// jitter in [0.5x, 1.0x] of the deterministic component.
func backoffDelay(attempt int, base, maxDelay time.Duration, r *rand.Rand) time.Duration {
	deterministic := base << attempt
	if deterministic <= 0 || deterministic > maxDelay {
		deterministic = maxDelay
	}
	jitter := 0.5 + r.Float64()*0.5
	out := time.Duration(float64(deterministic) * jitter)
	if out > maxDelay {
		out = maxDelay
	}
	if out < base/2 {
		out = base / 2
	}
	return out
}

// supervisor runs the dial → run → reconnect loop on its own goroutine.
type supervisor struct {
	cfg    Config
	holder *connHolder
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	rng    *rand.Rand

	statusMu sync.Mutex
	status   ConnectionState
}

// newSupervisor wires the supervisor; Start launches the goroutine.
func newSupervisor(parent context.Context, cfg Config, holder *connHolder) *supervisor {
	ctx, cancel := context.WithCancel(parent)
	return &supervisor{
		cfg:    cfg,
		holder: holder,
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
		//nolint:gosec // jitter randomness is non-cryptographic
		rng:    rand.New(rand.NewSource(cfg.Clock().UnixNano())),
		status: ConnectionState{Phase: PhaseDisconnected},
	}
}

func (s *supervisor) start() {
	go s.run()
}

func (s *supervisor) stop() {
	s.cancel()
	<-s.done
	s.holder.close()
}

func (s *supervisor) run() {
	defer close(s.done)

	epIdx := 0
	attempt := 0
	var lastConnect time.Time

	for {
		if err := s.ctx.Err(); err != nil {
			s.setStatus(PhaseClosed, TrackerEndpoint{}, err, time.Time{})
			return
		}
		ep := s.cfg.Endpoints[epIdx%len(s.cfg.Endpoints)]
		s.setStatus(PhaseConnecting, ep, nil, time.Time{})

		dialCtx, dialCancel := context.WithTimeout(s.ctx, s.cfg.DialTimeout)
		conn, err := s.cfg.Transport.Dial(dialCtx, transport.Endpoint{
			Addr:         ep.Addr,
			IdentityHash: ep.IdentityHash,
		}, signerAsIdentity{Signer: s.cfg.Identity})
		dialCancel()

		if err != nil {
			delay := backoffDelay(attempt, s.cfg.BackoffBase, s.cfg.BackoffMax, s.rng)
			attemptAt := s.cfg.Clock().Add(delay)
			s.setStatus(PhaseDisconnected, ep, err, attemptAt)
			attempt++
			epIdx++
			select {
			case <-time.After(delay):
			case <-s.ctx.Done():
				s.setStatus(PhaseClosed, ep, s.ctx.Err(), time.Time{})
				return
			}
			continue
		}

		// connected
		s.holder.set(conn)
		lastConnect = s.cfg.Clock()
		s.setStatus(PhaseConnected, ep, nil, time.Time{})
		s.cfg.Logger.Info().Str("addr", ep.Addr).Msg("trackerclient: connected")

		hbErrCh := make(chan error, 1)
		hbCtx, hbCancel := context.WithCancel(s.ctx)
		go runHeartbeat(hbCtx, conn, s.cfg.HeartbeatPeriod, s.cfg.HeartbeatMisses, s.cfg.MaxFrameSize, func(err error) {
			select {
			case hbErrCh <- err:
			default:
			}
		})

		if s.cfg.OfferHandler != nil || s.cfg.SettlementHandler != nil {
			go runPushAcceptor(s.ctx, conn, s.cfg.OfferHandler, s.cfg.SettlementHandler, s.cfg.MaxFrameSize)
		}

		select {
		case <-conn.Done():
		case err := <-hbErrCh:
			s.cfg.Logger.Warn().Err(err).Msg("trackerclient: heartbeat lost")
			_ = conn.Close()
		case <-s.ctx.Done():
			hbCancel()
			_ = conn.Close()
			s.holder.clear()
			s.setStatus(PhaseClosed, ep, s.ctx.Err(), time.Time{})
			return
		}
		hbCancel()

		s.holder.clear()
		s.setStatus(PhaseDisconnected, ep, ErrConnectionLost, time.Time{})

		// reset the attempt counter only if we were stably connected ≥ 30s
		if s.cfg.Clock().Sub(lastConnect) >= 30*time.Second {
			attempt = 0
		} else {
			attempt++
		}
		epIdx++
	}
}

func (s *supervisor) setStatus(phase ConnectionPhase, ep TrackerEndpoint, lastErr error, nextAt time.Time) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.status.Phase = phase
	s.status.Endpoint = ep
	s.status.LastError = lastErr
	s.status.NextAttemptAt = nextAt
	if phase == PhaseConnected {
		s.status.ConnectedAt = s.cfg.Clock()
	}
}

func (s *supervisor) snapshot() ConnectionState {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	return s.status
}

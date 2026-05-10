package seederflow

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
)

// advertisePeriod is the heartbeat between Advertise calls. Plugin spec
// §6.3 says state changes "take effect within one heartbeat"; 30s
// matches the trackerclient heartbeat order of magnitude.
const advertisePeriod = 30 * time.Second

// Run starts the coordinator's background loops:
//
//  1. Boot conformance gate (cfg.ConformanceFn).
//  2. Initial Advertise reflecting current Available().
//  3. Accept loop on cfg.Acceptor; each accepted *TunnelConn dispatches
//     to Serve, matching against the only outstanding reservation
//     (single-flight v1).
//  4. Periodic re-advertise heartbeat.
//
// Run returns nil on ctx-cancel after both loops exit. Run is single-shot:
// the second call returns ErrAlreadyStarted.
func (c *Coordinator) Run(ctx context.Context) error {
	c.startMu.Lock()
	if c.closed {
		c.startMu.Unlock()
		return ErrClosed
	}
	if c.started {
		c.startMu.Unlock()
		return ErrAlreadyStarted
	}
	c.started = true
	c.startMu.Unlock()

	// Boot conformance — failure is logged but does not abort Run; the
	// availability gate refuses HandleOffer and Advertise will report
	// available=false until ResetConformance is called.
	if err := c.RunConformance(ctx); err != nil {
		c.cfg.Logger.Warn().Err(err).Msg("seederflow: boot conformance failed; seeder advertise will report unavailable")
	}

	// Initial advertise.
	c.advertise(ctx)

	var wg sync.WaitGroup
	wg.Add(2)
	go c.acceptLoop(ctx, &wg)
	go c.advertiseLoop(ctx, &wg)
	wg.Wait()
	return nil
}

func (c *Coordinator) acceptLoop(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		conn, err := c.cfg.Acceptor.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			c.cfg.Logger.Debug().Err(err).Msg("seederflow: tunnel accept")
			// Avoid a tight loop on a degenerate acceptor; the
			// production listener returns context-derived errors on
			// shutdown which we already handle above.
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		envHash, ok := c.peekReservation()
		if !ok {
			_ = conn.SendError("seederflow: no outstanding reservation")
			_ = conn.CloseWrite()
			_ = conn.Close()
			continue
		}
		go func(conn TunnelConn, hash [32]byte) {
			if err := c.Serve(ctx, conn, hash); err != nil {
				c.cfg.Logger.Warn().Err(err).Msg("seederflow: serve")
			}
		}(conn, envHash)
	}
}

// peekReservation returns the EnvelopeHash of the single outstanding
// reservation when exactly one exists. The v1 multiplexing strategy is
// "single-flight per seeder"; richer matching arrives once OfferPush
// carries a request_body hash or per-offer correlator.
func (c *Coordinator) peekReservation() ([32]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reservations) != 1 {
		return [32]byte{}, false
	}
	for k := range c.reservations {
		raw, _ := hex.DecodeString(k)
		var out [32]byte
		copy(out[:], raw)
		return out, true
	}
	return [32]byte{}, false
}

func (c *Coordinator) advertiseLoop(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	t := time.NewTicker(advertisePeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.advertise(ctx)
		}
	}
}

// advertise sends one Advertisement reflecting the coordinator's
// current availability. Errors are logged, not propagated — the
// trackerclient owns reconnect, and a single-call failure is harmless.
func (c *Coordinator) advertise(ctx context.Context) {
	now := c.cfg.Clock()
	available := c.Available(now)
	headroom := float32(0)
	if available {
		headroom = 1.0
	}
	if err := c.usageReporter().Advertise(ctx, &trackerclient.Advertisement{
		Models:     c.cfg.Models,
		MaxContext: c.cfg.MaxContext,
		Available:  available,
		Headroom:   headroom,
		Tiers:      c.cfg.Tiers,
	}); err != nil {
		c.cfg.Logger.Debug().Err(err).Msg("seederflow: advertise")
	}
}

// Close releases coordinator resources. Idempotent. Run-loop callers
// cancel ctx separately; Close itself does not unblock Run.
func (c *Coordinator) Close() error {
	c.startMu.Lock()
	if c.closed {
		c.startMu.Unlock()
		return nil
	}
	c.closed = true
	c.startMu.Unlock()
	if err := c.cfg.Acceptor.Close(); err != nil {
		return fmt.Errorf("seederflow: close acceptor: %w", err)
	}
	return nil
}

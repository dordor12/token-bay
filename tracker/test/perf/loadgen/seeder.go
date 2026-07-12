//go:build perf

package loadgen

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"time"

	quicgo "github.com/quic-go/quic-go"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
	"github.com/token-bay/token-bay/tracker/internal/broker"
	"github.com/token-bay/token-bay/tracker/test/e2e/driver"
)

// SeederOpts configures one simulated seeder.
type SeederOpts struct {
	TrackerAddr string
	SPKI        [32]byte
	Transport   *quicgo.Transport

	// Model this seeder advertises and serves.
	Model string
	// Headroom advertised (admission supply contribution weight).
	Headroom float64

	HeartbeatEvery time.Duration
	AdvertiseEvery time.Duration
	// ServeDelay simulates the time a real seeder spends serving before
	// reporting usage.
	ServeDelay time.Duration

	Prices   *broker.PriceTable
	Counters *Counters
}

// SimSeeder is a goroutine-hosted seeder: enrolls, advertises
// availability, answers offer pushes with an ephemeral-key accept, and
// files the ephemeral-key-signed UsageReport a real seeder sends after
// serving — without any tunnel (spec §3: the tunnel never touches the
// tracker).
type SimSeeder struct {
	cl   *client
	opts SeederOpts
}

// StartSeeder dials, enrolls (role seeder), advertises, and starts the
// heartbeat / re-advertise / offer-push loops.
func StartSeeder(ctx context.Context, opts SeederOpts) (*SimSeeder, error) {
	cl, err := newClient(ctx, opts.Transport, opts.TrackerAddr, opts.SPKI, roleSeeder, opts.Counters)
	if err != nil {
		return nil, err
	}
	s := &SimSeeder{cl: cl, opts: opts}
	if err := s.advertise(ctx); err != nil {
		cl.close()
		return nil, err
	}
	go cl.heartbeatLoop(ctx, opts.HeartbeatEvery)
	go s.advertiseLoop(ctx)
	go s.offerLoop(ctx)
	return s, nil
}

// Close tears the seeder down.
func (s *SimSeeder) Close() { s.cl.close() }

func (s *SimSeeder) advertise(ctx context.Context) error {
	resp, err := s.cl.callMsg(ctx, tbproto.RpcMethod_RPC_METHOD_ADVERTISE, &tbproto.Advertisement{
		Models:     []string{s.opts.Model},
		MaxContext: 200_000,
		Available:  true,
		Headroom:   float32(s.opts.Headroom),
		Tiers:      0x1, // STANDARD
	})
	if err != nil {
		return fmt.Errorf("loadgen: ADVERTISE transport: %w", err)
	}
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		return fmt.Errorf("loadgen: ADVERTISE status=%s error=%v", resp.Status, resp.GetError())
	}
	return nil
}

// advertiseLoop re-asserts availability across tracker restarts; the
// server registers seeders on connect, so this is freshness, not
// correctness.
func (s *SimSeeder) advertiseLoop(ctx context.Context) {
	t := time.NewTicker(s.opts.AdvertiseEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.cl.gone:
			return
		case <-t.C:
		}
		if err := s.advertise(ctx); err != nil {
			s.cl.markDead()
			return
		}
	}
}

// offerLoop accepts server-initiated push streams. Offers are answered
// inline (the tracker's offer_timeout_ms clock is running); the usage
// report runs on its own goroutine after the simulated serve delay.
func (s *SimSeeder) offerLoop(ctx context.Context) {
	for {
		stream, tag, err := s.cl.cli.AcceptPush(ctx)
		if err != nil {
			// Connection gone or run shutting down.
			s.cl.markDead()
			return
		}
		if tag != driver.PushTagOffer {
			// Seeders only expect offers; drain and drop anything else.
			_ = stream.Close()
			continue
		}
		s.handleOffer(ctx, stream)
	}
}

func (s *SimSeeder) handleOffer(ctx context.Context, stream *quicgo.Stream) {
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(rpcTimeout))

	var push tbproto.OfferPush
	if err := s.cl.cli.ReadPush(stream, &push); err != nil {
		s.cl.c.Op(true)
		return
	}

	ephPub, ephPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		_ = s.cl.cli.WritePush(stream, &tbproto.OfferDecision{Accept: false, RejectReason: "keygen"})
		s.cl.c.Op(true)
		return
	}
	if err := s.cl.cli.WritePush(stream, &tbproto.OfferDecision{
		Accept:          true,
		EphemeralPubkey: ephPub,
	}); err != nil {
		s.cl.c.Op(true)
		return
	}

	go s.reportUsage(ctx, &push, ephPriv)
}

// reportUsage files the post-serve UsageReport: token counts pinned to
// exactly what the offer reserved (actual == reserved keeps the
// settlement overspend guard satisfied for every request shape), cost
// recomputed through the broker's own PriceTable, assertion signed
// with the PER-OFFER ephemeral key.
func (s *SimSeeder) reportUsage(ctx context.Context, push *tbproto.OfferPush, ephPriv ed25519.PrivateKey) {
	if !sleepCtx(ctx, s.opts.ServeDelay) {
		return
	}
	in, out := push.MaxInputTokens, push.MaxOutputTokens
	cost, err := s.opts.Prices.ActualCost(push.Model, in, out)
	if err != nil {
		s.cl.c.Op(true)
		return
	}
	seederID := s.cl.cli.IdentityID()
	sig, err := signing.SignUsageAssertion(ephPriv, signing.UsageAssertion{
		RequestID:    push.RequestId,
		ConsumerID:   push.ConsumerId,
		SeederID:     seederID[:],
		Model:        push.Model,
		InputTokens:  in,
		OutputTokens: out,
		CostCredits:  cost,
	})
	if err != nil {
		s.cl.c.Op(true)
		return
	}
	resp, err := s.cl.callMsg(ctx, tbproto.RpcMethod_RPC_METHOD_USAGE_REPORT, &tbproto.UsageReport{
		RequestId:    push.RequestId,
		InputTokens:  in,
		OutputTokens: out,
		Model:        push.Model,
		SeederSig:    sig,
	})
	if err != nil || resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		if err == nil {
			// Transport already counted by callMsg; count the non-OK.
			s.cl.c.ErrorOps.Add(1)
		}
		return
	}
	s.cl.c.UsageReportsSent.Add(1)
}

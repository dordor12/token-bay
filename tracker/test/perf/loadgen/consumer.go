//go:build perf

package loadgen

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"math/rand/v2"
	"time"

	quicgo "github.com/quic-go/quic-go"
	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/broker"
	"github.com/token-bay/token-bay/tracker/test/e2e/driver"
)

// ConsumerOpts configures one simulated consumer.
type ConsumerOpts struct {
	TrackerAddr string
	SPKI        [32]byte
	Transport   *quicgo.Transport

	Model  string
	MaxIn  uint64
	MaxOut uint64

	RequestEvery   time.Duration
	HeartbeatEvery time.Duration

	Prices   *broker.PriceTable
	Counters *Counters
}

// SimConsumer is a goroutine-hosted consumer: enrolls for the starter
// grant, then paces BALANCE → signed envelope → BROKER_REQUEST cycles
// and counter-signs settlement pushes. It stops requesting (but keeps
// settling) once its balance can't cover another request's max cost —
// budget exhaustion is a valid outcome, not an error (spec §3).
type SimConsumer struct {
	cl   *client
	opts ConsumerOpts
}

// StartConsumer dials, enrolls (role consumer), and starts the
// heartbeat / settlement-push / request loops. lifeCtx spans the
// client's whole lifetime; reqCtx bounds only the request pacing —
// the scenario cancels reqCtx at the end of the measured window and
// keeps lifeCtx alive through the drain grace so in-flight
// settlements still get counter-signed.
func StartConsumer(lifeCtx, reqCtx context.Context, opts ConsumerOpts) (*SimConsumer, error) {
	cl, err := newClient(lifeCtx, opts.Transport, opts.TrackerAddr, opts.SPKI, roleConsumer, opts.Counters)
	if err != nil {
		return nil, err
	}
	c := &SimConsumer{cl: cl, opts: opts}
	go cl.heartbeatLoop(lifeCtx, opts.HeartbeatEvery)
	go c.settlementLoop(lifeCtx)
	go c.requestLoop(reqCtx)
	return c, nil
}

// Close tears the consumer down.
func (c *SimConsumer) Close() { c.cl.close() }

// requestLoop paces one broker request per RequestEvery with a random
// initial phase (10k consumers must not fire in lockstep) and ±10%
// jitter per cycle.
func (c *SimConsumer) requestLoop(ctx context.Context) {
	if !sleepCtx(ctx, time.Duration(rand.Int64N(int64(c.opts.RequestEvery)))) { //nolint:gosec // G404: pacing jitter, not crypto
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.cl.gone:
			return
		default:
		}
		if done := c.requestOnce(ctx); done {
			return
		}
		jitter := time.Duration(rand.Int64N(int64(c.opts.RequestEvery) / 5)) //nolint:gosec // G404: pacing jitter, not crypto
		if !sleepCtx(ctx, c.opts.RequestEvery-c.opts.RequestEvery/10+jitter) {
			return
		}
	}
}

// requestOnce runs one BALANCE → envelope → BROKER_REQUEST cycle.
// Returns true when the loop should stop (budget exhausted or the
// connection died).
func (c *SimConsumer) requestOnce(ctx context.Context) (stop bool) {
	maxCost, err := c.opts.Prices.ActualCost(c.opts.Model, uint32(c.opts.MaxIn), uint32(c.opts.MaxOut)) //nolint:gosec // perf token counts are tiny
	if err != nil {
		c.cl.c.Op(true)
		return true
	}

	// BALANCE — also the freshness-bounded proof the envelope carries.
	c.cl.rpcMu.Lock()
	bctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	snap, err := c.cl.cli.VerifiedBalance(bctx, c.cl.cli.IdentityID())
	cancel()
	c.cl.rpcMu.Unlock()
	c.cl.c.Op(err != nil)
	if err != nil {
		c.cl.markDead()
		return true
	}
	if driver.BalanceCredits(snap) < int64(maxCost) { //nolint:gosec // tiny cost
		c.cl.c.BudgetExhausted.Add(1)
		return true
	}

	env, err := buildSignedBrokerEnvelope(c.cl.cli, c.opts.Model, c.opts.MaxIn, c.opts.MaxOut, snap)
	if err != nil {
		c.cl.c.Op(true)
		return false
	}

	start := time.Now()
	resp, err := c.cl.call(ctx, tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST, env)
	c.cl.c.BrokerLatency.Observe(time.Since(start))
	if err != nil {
		c.cl.markDead()
		return true
	}
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		// Policy turn-downs (frozen, admission hard-reject) come back as
		// non-OK statuses; under load they are rejections, not harness
		// errors — but they were already counted as a clean op, so only
		// classify, don't double count.
		c.cl.c.Rejected.Add(1)
		return false
	}

	var brr tbproto.BrokerRequestResponse
	if err := proto.Unmarshal(resp.Payload, &brr); err != nil {
		c.cl.c.ErrorOps.Add(1)
		return false
	}
	switch {
	case brr.GetSeederAssignment() != nil:
		c.cl.c.Assigned.Add(1)
		c.cl.c.RecordAssignment(brr.GetSeederAssignment().GetReservationToken())
	case brr.GetQueued() != nil:
		c.cl.c.Queued.Add(1)
	case brr.GetNoCapacity() != nil:
		c.cl.c.NoCapacity.Add(1)
	case brr.GetRejected() != nil:
		c.cl.c.Rejected.Add(1)
	default:
		c.cl.c.ErrorOps.Add(1)
	}
	return false
}

// settlementLoop accepts settlement pushes: verify the preimage hash,
// counter-sign the RAW preimage with the identity key, deliver the sig
// via the unary SETTLE RPC, then ack the push stream.
func (c *SimConsumer) settlementLoop(ctx context.Context) {
	for {
		stream, tag, err := c.cl.cli.AcceptPush(ctx)
		if err != nil {
			c.cl.markDead()
			return
		}
		if tag != driver.PushTagSettlement {
			_ = stream.Close()
			continue
		}
		c.handleSettlement(ctx, stream)
	}
}

func (c *SimConsumer) handleSettlement(ctx context.Context, stream *quicgo.Stream) {
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(rpcTimeout))

	var push tbproto.SettlementPush
	if err := c.cl.cli.ReadPush(stream, &push); err != nil {
		c.cl.c.Op(true)
		return
	}
	sum := sha256.Sum256(push.PreimageBody)
	if len(push.PreimageHash) != len(sum) || string(push.PreimageHash) != string(sum[:]) {
		c.cl.c.Op(true)
		return // never counter-sign a mismatched preimage
	}

	// Counter-sign the RAW preimage bytes with the identity key — the
	// tracker verifies against the mTLS-connection pubkey.
	sig := ed25519.Sign(c.cl.cli.PrivateKey(), push.PreimageBody)
	resp, err := c.cl.callMsg(ctx, tbproto.RpcMethod_RPC_METHOD_SETTLE, &tbproto.SettleRequest{
		PreimageHash: push.PreimageHash,
		ConsumerSig:  sig,
	})
	if err != nil {
		return
	}
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		c.cl.c.ErrorOps.Add(1)
		return
	}
	c.cl.c.SettlementsSigned.Add(1)
	_ = c.cl.cli.WritePush(stream, &tbproto.SettleAck{})
}

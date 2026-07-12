//go:build perf

package loadgen

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	quicgo "github.com/quic-go/quic-go"
	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/test/e2e/driver"
)

// Enroll role bits (EnrollRequest.Role: bit0 consumer, bit1 seeder).
const (
	roleConsumer uint32 = 0x1
	roleSeeder   uint32 = 0x2
)

// rpcTimeout bounds every individual unary RPC a simulated client
// issues. Generous: under full load a busy tracker may queue.
const rpcTimeout = 15 * time.Second

// client is the shared base of SimConsumer and SimSeeder: one dialed
// RPCClient, a mutex serializing unary Calls (RPCClient's contract is
// single-caller), and the run-wide counters.
type client struct {
	cli *driver.RPCClient
	c   *Counters

	// rpcMu serializes unary RPCs; the heartbeat stream has its own
	// single owner goroutine and needs no lock.
	rpcMu sync.Mutex

	// dead flips once the connection is unusable; loops exit quietly.
	dead sync.Once
	gone chan struct{}
}

// newClient dials trackerAddr through tr with a fresh identity and
// enrolls it under role. Both steps are counted operations.
func newClient(ctx context.Context, tr *quicgo.Transport, trackerAddr string, spki [32]byte, role uint32, c *Counters) (*client, error) {
	dctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	priv := newIdentity()
	cli, err := driver.DialRPCVia(dctx, tr, trackerAddr, spki, priv)
	c.Op(err != nil)
	if err != nil {
		return nil, fmt.Errorf("loadgen: dial %s: %w", trackerAddr, err)
	}

	cl := &client{cli: cli, c: c, gone: make(chan struct{})}
	if err := cl.enroll(ctx, role); err != nil {
		_ = cli.Close()
		return nil, err
	}
	return cl, nil
}

func newIdentity() ed25519.PrivateKey {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		// crypto/rand failure is unrecoverable for the whole process.
		panic(fmt.Sprintf("loadgen: generate identity: %v", err))
	}
	return priv
}

// enroll sends ENROLL for the client's identity; the tracker answers
// with the starter grant.
func (cl *client) enroll(ctx context.Context, role uint32) error {
	pub, ok := cl.cli.PrivateKey().Public().(ed25519.PublicKey)
	if !ok {
		return fmt.Errorf("loadgen: identity key is not Ed25519")
	}
	fp := sha256.Sum256(append([]byte("perf-fingerprint-"), pub...))
	resp, err := cl.callMsg(ctx, tbproto.RpcMethod_RPC_METHOD_ENROLL, &tbproto.EnrollRequest{
		IdentityPubkey:     pub,
		Role:               role,
		AccountFingerprint: fp[:],
	})
	if err != nil {
		return fmt.Errorf("loadgen: ENROLL transport: %w", err)
	}
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		return fmt.Errorf("loadgen: ENROLL status=%s error=%v", resp.Status, resp.GetError())
	}
	return nil
}

// callMsg issues one counted, serialized, deadline-bounded unary RPC.
func (cl *client) callMsg(ctx context.Context, method tbproto.RpcMethod, msg proto.Message) (*tbproto.RpcResponse, error) {
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	cl.rpcMu.Lock()
	resp, err := cl.cli.CallMsg(rctx, method, msg)
	cl.rpcMu.Unlock()
	cl.c.Op(err != nil)
	return resp, err
}

// call is callMsg for a pre-marshaled payload.
func (cl *client) call(ctx context.Context, method tbproto.RpcMethod, payload []byte) (*tbproto.RpcResponse, error) {
	rctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	cl.rpcMu.Lock()
	resp, err := cl.cli.Call(rctx, method, payload)
	cl.rpcMu.Unlock()
	cl.c.Op(err != nil)
	return resp, err
}

// markDead flags the connection unusable exactly once.
func (cl *client) markDead() {
	cl.dead.Do(func() { close(cl.gone) })
}

// heartbeatLoop pings on the held heartbeat stream every interval
// (with a random initial phase so 10k clients don't ping in lockstep)
// until ctx cancels or the connection dies. Heartbeats keep the QUIC
// connection inside the server's 60s idle timeout and keep the seeder
// registry record fresh for admission supply weighting.
func (cl *client) heartbeatLoop(ctx context.Context, every time.Duration) {
	var seq uint64
	if !sleepCtx(ctx, time.Duration(rand.Int64N(int64(every)))) { //nolint:gosec // G404: scheduling jitter, not crypto
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-cl.gone:
			return
		case <-t.C:
		}
		seq++
		hctx, cancel := context.WithTimeout(ctx, rpcTimeout)
		err := cl.cli.Heartbeat(hctx, seq)
		cancel()
		if err != nil {
			// A failed ping means the connection is gone (or the run is
			// shutting down); either way this client is done.
			cl.markDead()
			return
		}
	}
}

// close tears the QUIC connection down.
func (cl *client) close() {
	cl.markDead()
	_ = cl.cli.Close()
}

// sleepCtx sleeps d or until ctx cancels; reports false on cancel.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

package federation

import (
	"context"
	"crypto/ed25519"
	"math/rand"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
)

// Dialer drives one allowlisted peer with a redial-with-backoff loop.
// One Dialer per peer; spawned by Federation.Open as a goroutine.
//
// On every iteration the dialer tries to Dial; on success it hands the
// PeerConn to OnConnected, which MUST block until the connection drops,
// then return so the dialer reconnects. ctx cancellation exits cleanly
// at any point, including mid-sleep.
type Dialer struct {
	Transport   Transport
	Peer        AllowlistedPeer
	MyTrackerID ids.TrackerID
	MyPriv      ed25519.PrivateKey
	HandshakeTO time.Duration
	BackoffBase time.Duration
	BackoffMax  time.Duration
	Now         func() time.Time

	// OnConnected runs the federation-level handshake on top of the
	// supplied PeerConn (in production: f.attachAndWait). It MUST block
	// until the conn drops, then return so the dialer can reconnect.
	OnConnected func(c PeerConn)

	// OnFailure is an optional metric sink. nil disables.
	OnFailure func(reason string, err error)
}

// Run blocks until ctx is canceled, looping Dial → OnConnected → backoff.
func (d *Dialer) Run(ctx context.Context) {
	wait := d.BackoffBase
	if wait <= 0 {
		wait = time.Second
	}
	maxWait := d.BackoffMax
	if maxWait < wait {
		maxWait = wait
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		conn, err := d.Transport.Dial(ctx, d.Peer.Addr, d.Peer.PubKey)
		if err != nil {
			if d.OnFailure != nil {
				d.OnFailure("dial", err)
			}
			if !sleepWithJitter(ctx, wait) {
				return
			}
			wait *= 2
			if wait > maxWait {
				wait = maxWait
			}
			continue
		}

		// Reset backoff on a successful dial.
		wait = d.BackoffBase
		if wait <= 0 {
			wait = time.Second
		}

		// Hand off to OnConnected; blocks until the conn drops.
		d.OnConnected(conn)
	}
}

// sleepWithJitter waits up to base*rand[0,1) before returning true, or
// returns false if ctx fires.
func sleepWithJitter(ctx context.Context, base time.Duration) bool {
	if base <= 0 {
		return true
	}
	jittered := time.Duration(rand.Int63n(int64(base) + 1)) //nolint:gosec // not security-sensitive
	if jittered <= 0 {
		jittered = time.Millisecond
	}
	t := time.NewTimer(jittered)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

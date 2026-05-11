package ledger

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
)

// merkleRootSignDomain is the per-protocol domain separator mixed into
// the (hour, root) signing preimage. Prevents cross-protocol replay of
// the tracker's Ed25519 signature into another Token-Bay message family
// that might happen to be 8 + 32 = 40 bytes of opaque payload.
const merkleRootSignDomain = "token-bay/merkle-root\x00"

// merkleHash returns the binary Merkle root over leaves (each a 32-byte
// SHA-256 entry hash). Returns nil on an empty slice — the orchestrator
// must not persist a root for an empty hour.
//
// Algorithm: at each level, pair adjacent leaves; if the count is odd,
// duplicate the last leaf to make it even; SHA-256(left || right) yields
// the parent. Recurse upward until one node remains.
func merkleHash(leaves [][]byte) []byte {
	if len(leaves) == 0 {
		return nil
	}
	level := leaves
	for len(level) > 1 {
		if len(level)%2 == 1 {
			level = append(level, level[len(level)-1])
		}
		next := make([][]byte, 0, len(level)/2)
		for i := 0; i < len(level); i += 2 {
			h := sha256.New()
			h.Write(level[i])
			h.Write(level[i+1])
			next = append(next, h.Sum(nil))
		}
		level = next
	}
	return level[0]
}

// merkleRootSignPreimage returns the canonical bytes the tracker signs
// for an (hour, root) Merkle attestation. Format:
//
//	"token-bay/merkle-root\x00" || hour-big-endian-u64 || root
//
// Federation accepts the signature as opaque today (federation core
// design §5, line 327), so the orchestrator owns this canonical form.
func merkleRootSignPreimage(hour uint64, root []byte) []byte {
	out := make([]byte, 0, len(merkleRootSignDomain)+8+len(root))
	out = append(out, merkleRootSignDomain...)
	var hourBytes [8]byte
	binary.BigEndian.PutUint64(hourBytes[:], hour)
	out = append(out, hourBytes[:]...)
	out = append(out, root...)
	return out
}

// RunRollupOnce computes and persists the Merkle root + tracker signature
// for the closed-hour bucket `hour`. Idempotent in every direction:
//
//   - if a root already exists for hour: no-op
//   - if the storage write races a concurrent caller and loses (returning
//     ErrDuplicateMerkleRoot): no-op
//   - if the hour bucket is empty: skipped — no sentinel row, so
//     federation's ReadyRoot returns ok=false for the hour, which is
//     correct ("the tracker has nothing to attest")
func (l *Ledger) RunRollupOnce(ctx context.Context, hour uint64) error {
	if _, _, ok, err := l.store.GetMerkleRoot(ctx, hour); err != nil {
		return fmt.Errorf("ledger: rollup check existing root: %w", err)
	} else if ok {
		return nil
	}

	leaves, err := l.store.LeavesForHour(ctx, hour)
	if err != nil {
		return fmt.Errorf("ledger: rollup read leaves: %w", err)
	}
	if len(leaves) == 0 {
		return nil
	}

	root := merkleHash(leaves)
	if len(root) != sha256.Size {
		return fmt.Errorf("ledger: rollup unexpected root length %d", len(root))
	}

	sig := ed25519.Sign(l.trackerKey, merkleRootSignPreimage(hour, root))

	switch err := l.store.PutMerkleRoot(ctx, hour, root, sig); {
	case err == nil:
		return nil
	case errors.Is(err, storage.ErrDuplicateMerkleRoot):
		// Concurrent caller won the race; treat as no-op.
		return nil
	default:
		return fmt.Errorf("ledger: rollup put root hour=%d: %w", hour, err)
	}
}

// StartRollup spawns the hourly Merkle rollup goroutine. The ticker
// fires at `interval`; each tick computes prevHour = floor(now/3600) - 1
// from the Ledger's clock and calls RunRollupOnce. Idempotency in
// RunRollupOnce means a too-frequent ticker (or restart-then-retick on
// the same hour) is harmless.
//
// Mirrors admission.startAggregator: caller owns the lifecycle, Close
// drains via wg.Wait. Caller must invoke StartRollup at most once per
// Ledger.
//
// Non-positive interval is treated as a programmer error and skips the
// goroutine entirely — the orchestrator simply does not run.
func (l *Ledger) StartRollup(interval time.Duration) {
	if interval <= 0 {
		return
	}
	l.wg.Add(1)
	go l.rollupLoop(interval)
}

func (l *Ledger) rollupLoop(interval time.Duration) {
	defer l.wg.Done()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			hour, ok := prevHourFromTime(l.nowFn())
			if !ok {
				continue
			}
			// RunRollupOnce uses ctx for SQLite cancellation. Pair it
			// with stop so a Close mid-rollup doesn't block on a slow
			// SQLite write.
			ctx, cancel := stopCtx(l.stop)
			_ = l.RunRollupOnce(ctx, hour)
			cancel()
		}
	}
}

// prevHourFromTime returns floor(t.Unix()/3600) - 1, the just-completed
// hour bucket. Returns ok=false for clocks before 1970-01-01 02:00 UTC
// where the subtraction would underflow — never reachable in production
// but tests deserve a defined behavior.
func prevHourFromTime(t time.Time) (uint64, bool) {
	s := t.Unix()
	if s < 7200 {
		return 0, false
	}
	// s ≥ 7200 makes the int64→uint64 cast safe (gosec G115).
	return uint64(s)/3600 - 1, true
}

// stopCtx returns a context that's cancelled either by the caller-side
// cancel func or when `stop` is closed. Used by rollupLoop so a Close
// mid-rollup unblocks any in-flight SQLite query.
func stopCtx(stop <-chan struct{}) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

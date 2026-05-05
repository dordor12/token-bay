package trackerclient

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

func TestBalanceCacheHit(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	bc := newBalanceCache(2*time.Minute, func() time.Time { return now })
	fresh := &tbproto.SignedBalanceSnapshot{Body: &tbproto.BalanceSnapshotBody{
		ExpiresAt: uint64(now.Add(10 * time.Minute).Unix()),
	}}

	var calls int32
	fn := func(_ context.Context, _ ids.IdentityID) (*tbproto.SignedBalanceSnapshot, error) {
		atomic.AddInt32(&calls, 1)
		return fresh, nil
	}

	id := ids.IdentityID{1}
	s1, err := bc.Get(context.Background(), id, fn)
	require.NoError(t, err)
	s2, err := bc.Get(context.Background(), id, fn)
	require.NoError(t, err)
	assert.Same(t, s1, s2)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls))
}

func TestBalanceCacheMissStale(t *testing.T) {
	var nowMu sync.Mutex
	now := time.Unix(1_000_000, 0)
	clock := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	bc := newBalanceCache(2*time.Minute, clock)
	snap := &tbproto.SignedBalanceSnapshot{Body: &tbproto.BalanceSnapshotBody{
		ExpiresAt: uint64(now.Add(60 * time.Second).Unix()), // within headroom
	}}

	var calls int32
	fn := func(_ context.Context, _ ids.IdentityID) (*tbproto.SignedBalanceSnapshot, error) {
		atomic.AddInt32(&calls, 1)
		return snap, nil
	}
	_, _ = bc.Get(context.Background(), ids.IdentityID{}, fn)
	_, _ = bc.Get(context.Background(), ids.IdentityID{}, fn)
	assert.GreaterOrEqual(t, atomic.LoadInt32(&calls), int32(2),
		"expected re-fetch when within ttlHeadroom of expiry")
}

func TestBalanceCacheSingleflight(t *testing.T) {
	bc := newBalanceCache(time.Minute, time.Now)
	var calls int32
	fn := func(_ context.Context, _ ids.IdentityID) (*tbproto.SignedBalanceSnapshot, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(50 * time.Millisecond)
		return &tbproto.SignedBalanceSnapshot{Body: &tbproto.BalanceSnapshotBody{
			ExpiresAt: uint64(time.Now().Add(time.Hour).Unix()),
		}}, nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = bc.Get(context.Background(), ids.IdentityID{}, fn)
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls))
}

func TestBalanceCachePropagatesError(t *testing.T) {
	bc := newBalanceCache(time.Minute, time.Now)
	fn := func(_ context.Context, _ ids.IdentityID) (*tbproto.SignedBalanceSnapshot, error) {
		return nil, errors.New("boom")
	}
	_, err := bc.Get(context.Background(), ids.IdentityID{}, fn)
	require.Error(t, err)
}

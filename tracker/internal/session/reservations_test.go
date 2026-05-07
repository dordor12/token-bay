package session

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
)

func TestReserveAndRelease_HappyPath(t *testing.T) {
	r := NewReservations()
	consumer := ids.IdentityID{1}
	reqID := [16]byte{0xAA}
	err := r.Reserve(reqID, consumer, 100, 200, time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, uint64(100), r.Reserved(consumer))
	c, amt, ok := r.Release(reqID)
	require.True(t, ok)
	require.Equal(t, consumer, c)
	require.Equal(t, uint64(100), amt)
	require.Equal(t, uint64(0), r.Reserved(consumer))
}

func TestReserve_OverCommit(t *testing.T) {
	r := NewReservations()
	consumer := ids.IdentityID{1}
	require.NoError(t, r.Reserve([16]byte{1}, consumer, 100, 150, time.Now().Add(time.Minute)))
	err := r.Reserve([16]byte{2}, consumer, 100, 150, time.Now().Add(time.Minute))
	require.ErrorIs(t, err, ErrInsufficientCredits)
	require.Equal(t, uint64(100), r.Reserved(consumer))
}

func TestReserve_DuplicateReqID(t *testing.T) {
	r := NewReservations()
	require.NoError(t, r.Reserve([16]byte{1}, ids.IdentityID{1}, 10, 100, time.Now().Add(time.Minute)))
	err := r.Reserve([16]byte{1}, ids.IdentityID{1}, 5, 100, time.Now().Add(time.Minute))
	require.ErrorIs(t, err, ErrDuplicateReservation)
}

func TestRelease_Idempotent(t *testing.T) {
	r := NewReservations()
	require.NoError(t, r.Reserve([16]byte{1}, ids.IdentityID{1}, 10, 100, time.Now().Add(time.Minute)))
	_, _, ok := r.Release([16]byte{1})
	require.True(t, ok)
	_, _, ok = r.Release([16]byte{1})
	require.False(t, ok)
}

func TestSweepExpired_TTL(t *testing.T) {
	r := NewReservations()
	now := time.Now()
	require.NoError(t, r.Reserve([16]byte{1}, ids.IdentityID{1}, 10, 100, now.Add(-time.Second)))
	require.NoError(t, r.Reserve([16]byte{2}, ids.IdentityID{2}, 5, 100, now.Add(time.Hour)))
	expired := r.SweepExpired(now)
	require.Len(t, expired, 1)
	require.Equal(t, [16]byte{1}, expired[0].ReqID)
}

func TestReservations_GetAndSnapshot(t *testing.T) {
	r := NewReservations()
	consumer := ids.IdentityID{0xAA}
	require.NoError(t, r.Reserve([16]byte{1}, consumer, 50, 100, time.Now().Add(time.Hour)))
	require.NoError(t, r.Reserve([16]byte{2}, consumer, 25, 100, time.Now().Add(time.Hour)))

	got, ok := r.Get([16]byte{1})
	require.True(t, ok)
	require.Equal(t, consumer, got.ConsumerID)
	require.Equal(t, uint64(50), got.Amount)

	_, ok = r.Get([16]byte{99})
	require.False(t, ok)

	snap := r.Snapshot()
	require.Len(t, snap, 2)
}

func TestReservations_Concurrent_RaceClean(t *testing.T) {
	r := NewReservations()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := [16]byte{byte(i)}
			_ = r.Reserve(id, ids.IdentityID{byte(i)}, 1, 10, time.Now().Add(time.Minute))
			_, _, _ = r.Release(id)
		}(i)
	}
	wg.Wait()
}

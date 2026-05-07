package session

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
)

func TestNew_ConstructsEmptyStores(t *testing.T) {
	m := New()
	require.NotNil(t, m.Inflight)
	require.NotNil(t, m.Reservations)
	require.Equal(t, uint64(0), m.Reservations.Reserved(ids.IdentityID{1}))
	_, ok := m.Inflight.Get([16]byte{1})
	require.False(t, ok)
}

func TestSweepExpiredAndFail_TransitionsAssignedToFailed(t *testing.T) {
	m := New()
	consumer := ids.IdentityID{0xCC}
	seeder := ids.IdentityID{0xDD}
	reqID := [16]byte{0x01}

	m.Inflight.Insert(&Request{
		RequestID:      reqID,
		ConsumerID:     consumer,
		AssignedSeeder: seeder,
		State:          StateAssigned,
	})
	require.NoError(t, m.Reservations.Reserve(reqID, consumer, 100, 1000, time.Now().Add(-time.Second)))

	expired := m.SweepExpiredAndFail(time.Now())
	require.Len(t, expired, 1)
	require.Equal(t, reqID, expired[0].ReqID)
	require.Equal(t, consumer, expired[0].ConsumerID)
	require.Equal(t, uint64(100), expired[0].Amount)
	require.Equal(t, StateAssigned, expired[0].PriorState)
	require.Equal(t, seeder, expired[0].AssignedSeeder)

	got, ok := m.Inflight.Get(reqID)
	require.True(t, ok)
	require.Equal(t, StateFailed, got.State)
	require.Equal(t, uint64(0), m.Reservations.Reserved(consumer))
}

func TestSweepExpiredAndFail_SelectingHasNoSeeder(t *testing.T) {
	m := New()
	reqID := [16]byte{0x02}
	m.Inflight.Insert(&Request{RequestID: reqID, State: StateSelecting})
	require.NoError(t, m.Reservations.Reserve(reqID, ids.IdentityID{1}, 1, 100, time.Now().Add(-time.Second)))
	expired := m.SweepExpiredAndFail(time.Now())
	require.Len(t, expired, 1)
	require.Equal(t, StateSelecting, expired[0].PriorState)
	require.Equal(t, ids.IdentityID{}, expired[0].AssignedSeeder)
	got, _ := m.Inflight.Get(reqID)
	require.Equal(t, StateFailed, got.State)
}

func TestSweepExpiredAndFail_SkipsServingAndCompleted(t *testing.T) {
	// SERVING / COMPLETED reservations may linger after a 7.16-style ledger
	// failure; SweepExpiredAndFail reports them but does NOT transition.
	m := New()
	serving := [16]byte{0x03}
	done := [16]byte{0x04}
	m.Inflight.Insert(&Request{RequestID: serving, State: StateServing})
	m.Inflight.Insert(&Request{RequestID: done, State: StateCompleted, TerminatedAt: time.Now()})
	require.NoError(t, m.Reservations.Reserve(serving, ids.IdentityID{1}, 1, 100, time.Now().Add(-time.Second)))
	require.NoError(t, m.Reservations.Reserve(done, ids.IdentityID{2}, 1, 100, time.Now().Add(-time.Second)))

	expired := m.SweepExpiredAndFail(time.Now())
	require.Len(t, expired, 2)
	for _, e := range expired {
		switch e.ReqID {
		case serving:
			require.Equal(t, StateServing, e.PriorState)
			require.Equal(t, ids.IdentityID{}, e.AssignedSeeder)
		case done:
			require.Equal(t, StateCompleted, e.PriorState)
			require.Equal(t, ids.IdentityID{}, e.AssignedSeeder)
		default:
			t.Fatalf("unexpected ReqID %x", e.ReqID)
		}
	}
	s, _ := m.Inflight.Get(serving)
	require.Equal(t, StateServing, s.State)
	d, _ := m.Inflight.Get(done)
	require.Equal(t, StateCompleted, d.State)
}

func TestSweepExpiredAndFail_NoExpired(t *testing.T) {
	m := New()
	require.NoError(t, m.Reservations.Reserve([16]byte{1}, ids.IdentityID{1}, 1, 100, time.Now().Add(time.Hour)))
	require.Nil(t, m.SweepExpiredAndFail(time.Now()))
}

func TestSweepExpiredAndFail_ReservationWithoutInflight(t *testing.T) {
	// A reservation slot present without a matching Inflight entry is
	// reported with PriorState == StateUnspecified and zero seeder. No
	// transition runs (there's nothing to transition).
	m := New()
	require.NoError(t, m.Reservations.Reserve([16]byte{1}, ids.IdentityID{1}, 1, 100, time.Now().Add(-time.Second)))
	expired := m.SweepExpiredAndFail(time.Now())
	require.Len(t, expired, 1)
	require.Equal(t, StateUnspecified, expired[0].PriorState)
}

func TestManager_RaceClean(t *testing.T) {
	m := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := [16]byte{byte(i)}
			m.Inflight.Insert(&Request{RequestID: id, State: StateAssigned, AssignedSeeder: ids.IdentityID{byte(i)}})
			_ = m.Reservations.Reserve(id, ids.IdentityID{byte(i)}, 1, 100, time.Now().Add(-time.Millisecond))
		}(i)
	}
	wg.Wait()
	_ = m.SweepExpiredAndFail(time.Now())
}

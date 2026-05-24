package session

import (
	"crypto/ed25519"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/shared/ids"
)

func TestInflight_InsertGet(t *testing.T) {
	f := NewInflight()
	req := &Request{RequestID: [16]byte{1}, State: StateSelecting}
	f.Insert(req)
	got, ok := f.Get([16]byte{1})
	require.True(t, ok)
	require.Same(t, req, got)
}

func TestInflight_Get_Missing(t *testing.T) {
	f := NewInflight()
	_, ok := f.Get([16]byte{99})
	require.False(t, ok)
}

func TestInflight_TransitionCAS(t *testing.T) {
	f := NewInflight()
	f.Insert(&Request{RequestID: [16]byte{1}, State: StateSelecting})
	now := time.Unix(1700000000, 0)
	require.NoError(t, f.Transition([16]byte{1}, StateSelecting, StateAssigned, now))
	require.ErrorIs(t, f.Transition([16]byte{1}, StateSelecting, StateAssigned, now), ErrIllegalTransition)
	got, _ := f.Get([16]byte{1})
	require.Equal(t, StateAssigned, got.State)
}

func TestInflight_Transition_Unknown(t *testing.T) {
	f := NewInflight()
	require.ErrorIs(t, f.Transition([16]byte{99}, StateSelecting, StateAssigned, time.Unix(1700000000, 0)), ErrUnknownRequest)
}

func TestInflight_MarkSeeder(t *testing.T) {
	f := NewInflight()
	f.Insert(&Request{RequestID: [16]byte{1}, State: StateSelecting})
	pub := ed25519.PublicKey(make([]byte, 32))
	require.NoError(t, f.MarkSeeder([16]byte{1}, ids.IdentityID{0xAA}, pub))
	got, _ := f.Get([16]byte{1})
	require.Equal(t, ids.IdentityID{0xAA}, got.AssignedSeeder)
	require.Equal(t, pub, got.SeederPubkey)
}

func TestInflight_IndexLookupByHash(t *testing.T) {
	f := NewInflight()
	req := &Request{RequestID: [16]byte{1}, State: StateServing}
	f.Insert(req)
	require.NoError(t, f.IndexByHash([16]byte{1}, [32]byte{0xAB}))
	got, ok := f.LookupByHash([32]byte{0xAB})
	require.True(t, ok)
	require.Same(t, req, got)
}

func TestInflight_LookupByHash_Missing(t *testing.T) {
	f := NewInflight()
	_, ok := f.LookupByHash([32]byte{0x99})
	require.False(t, ok)
}

func TestInflight_LookupAssignment(t *testing.T) {
	f := NewInflight()
	rid := [16]byte{7}
	consumer := ids.IdentityID{0x11}
	seeder := ids.IdentityID{0x22}

	// Unknown request → ok=false.
	_, _, ok := f.LookupAssignment(rid)
	require.False(t, ok)

	// Inserted but not yet assigned → ok=false.
	f.Insert(&Request{RequestID: rid, ConsumerID: consumer, State: StateSelecting})
	_, _, ok = f.LookupAssignment(rid)
	require.False(t, ok)

	// After MarkSeeder → returns (consumer, seeder, true).
	require.NoError(t, f.MarkSeeder(rid, seeder, ed25519.PublicKey(make([]byte, 32))))
	gotConsumer, gotSeeder, ok := f.LookupAssignment(rid)
	require.True(t, ok)
	require.Equal(t, consumer, gotConsumer)
	require.Equal(t, seeder, gotSeeder)
}

func TestInflight_SweepTerminal_RemovesTerminal(t *testing.T) {
	f := NewInflight()
	now := time.Now()
	completed := &Request{RequestID: [16]byte{1}, State: StateCompleted, TerminatedAt: now.Add(-11 * time.Minute)}
	fresh := &Request{RequestID: [16]byte{2}, State: StateCompleted, TerminatedAt: now}
	serving := &Request{RequestID: [16]byte{3}, State: StateServing, TerminatedAt: time.Time{}}
	f.Insert(completed)
	f.Insert(fresh)
	f.Insert(serving)
	swept := f.SweepTerminal(now, 10*time.Minute)
	require.Len(t, swept, 1)
	require.Equal(t, [16]byte{1}, swept[0].RequestID)
	_, ok := f.Get([16]byte{2})
	require.True(t, ok)
	_, ok = f.Get([16]byte{3})
	require.True(t, ok)
}

func TestInflight_SweepTerminal_RemovesByHashIndex(t *testing.T) {
	f := NewInflight()
	now := time.Now()
	req := &Request{RequestID: [16]byte{1}, State: StateCompleted, TerminatedAt: now.Add(-time.Hour)}
	f.Insert(req)
	require.NoError(t, f.IndexByHash([16]byte{1}, [32]byte{0xCD}))
	_ = f.SweepTerminal(now, 10*time.Minute)
	_, ok := f.LookupByHash([32]byte{0xCD})
	require.False(t, ok)
}

func TestInflight_EnsureSettleSig(t *testing.T) {
	f := NewInflight()
	f.Insert(&Request{RequestID: [16]byte{1}, State: StateAssigned})
	ch1, err := f.EnsureSettleSig([16]byte{1})
	require.NoError(t, err)
	require.NotNil(t, ch1)
	ch2, err := f.EnsureSettleSig([16]byte{1})
	require.NoError(t, err)
	require.Equal(t, ch1, ch2) // same channel returned on repeat

	_, err = f.EnsureSettleSig([16]byte{99})
	require.ErrorIs(t, err, ErrUnknownRequest)

	// Channel is observable on the request.
	got, _ := f.Get([16]byte{1})
	require.Equal(t, ch1, got.SettleSig)
}

func TestInflight_Snapshot(t *testing.T) {
	f := NewInflight()
	f.Insert(&Request{RequestID: [16]byte{1}, ConsumerID: ids.IdentityID{0xAA}, State: StateSelecting})
	f.Insert(&Request{RequestID: [16]byte{2}, ConsumerID: ids.IdentityID{0xBB}, State: StateAssigned, AssignedSeeder: ids.IdentityID{0xDD}})
	snap := f.Snapshot()
	require.Len(t, snap, 2)

	byID := map[[16]byte]InflightSummary{}
	for _, s := range snap {
		byID[s.RequestID] = s
	}
	require.Equal(t, StateSelecting, byID[[16]byte{1}].State)
	require.Equal(t, ids.IdentityID{0xDD}, byID[[16]byte{2}].AssignedSeeder)
}

func TestInflight_ForceFail(t *testing.T) {
	f := NewInflight()
	f.Insert(&Request{RequestID: [16]byte{1}, State: StateAssigned})
	now := time.Unix(1700000000, 0)
	prev, ok := f.ForceFail([16]byte{1}, now)
	require.True(t, ok)
	require.Equal(t, StateAssigned, prev)
	got, _ := f.Get([16]byte{1})
	require.Equal(t, StateFailed, got.State)
	require.Equal(t, now, got.TerminatedAt)

	_, ok = f.ForceFail([16]byte{99}, now)
	require.False(t, ok)
}

func TestInflight_RaceClean_Transition(t *testing.T) {
	f := NewInflight()
	f.Insert(&Request{RequestID: [16]byte{1}, State: StateServing})
	var wins int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.Transition([16]byte{1}, StateServing, StateCompleted, time.Unix(1700000000, 0)); err == nil {
				atomic.AddInt32(&wins, 1)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), wins)
}

// TestInflight_Transition_TerminalUsesInjectedClock pins the contract that
// Transition records TerminatedAt from the caller-supplied `now`, not
// time.Now(). This is what lets the broker test under a fake clock.
func TestInflight_Transition_TerminalUsesInjectedClock(t *testing.T) {
	f := NewInflight()
	f.Insert(&Request{RequestID: [16]byte{1}, State: StateServing})

	injected := time.Unix(1700000000, 0)
	require.NoError(t, f.Transition([16]byte{1}, StateServing, StateCompleted, injected))

	got, _ := f.Get([16]byte{1})
	require.Equal(t, StateCompleted, got.State)
	require.True(t, got.TerminatedAt.Equal(injected),
		"TerminatedAt should equal injected clock: got=%v want=%v", got.TerminatedAt, injected)
}

// TestInflight_Transition_NonTerminalIgnoresClock confirms TerminatedAt stays
// zero for non-terminal transitions regardless of the time passed.
func TestInflight_Transition_NonTerminalIgnoresClock(t *testing.T) {
	f := NewInflight()
	f.Insert(&Request{RequestID: [16]byte{1}, State: StateSelecting})

	require.NoError(t, f.Transition([16]byte{1}, StateSelecting, StateAssigned, time.Unix(1700000000, 0)))

	got, _ := f.Get([16]byte{1})
	require.Equal(t, StateAssigned, got.State)
	require.True(t, got.TerminatedAt.IsZero(),
		"TerminatedAt should remain zero for non-terminal transition: got=%v", got.TerminatedAt)
}

// TestInflight_Transition_BackwardsClockNoPanic exercises the case where the
// injected clock moves backwards between calls (legitimate when tests fake
// time, and also a tolerable real-world skew condition). Transitions must
// not panic; each terminal transition records its own caller-supplied time
// without regard to monotonicity across requests.
func TestInflight_Transition_BackwardsClockNoPanic(t *testing.T) {
	f := NewInflight()
	f.Insert(&Request{RequestID: [16]byte{1}, State: StateServing})
	f.Insert(&Request{RequestID: [16]byte{2}, State: StateServing})

	t1 := time.Unix(1700000000, 0)
	t2 := t1.Add(-time.Minute) // clock jumps backwards

	require.NoError(t, f.Transition([16]byte{1}, StateServing, StateCompleted, t1))
	require.NoError(t, f.Transition([16]byte{2}, StateServing, StateCompleted, t2))

	r1, _ := f.Get([16]byte{1})
	r2, _ := f.Get([16]byte{2})
	require.True(t, r1.TerminatedAt.Equal(t1))
	require.True(t, r2.TerminatedAt.Equal(t2))
}

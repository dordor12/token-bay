package admission

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSupply_PrePublish_ReturnsZeroSnapshotMarker(t *testing.T) {
	s, _ := openTempSubsystem(t)
	snap := s.Supply()
	require.NotNil(t, snap, "Supply must never return nil — callers always get a snapshot")
	assert.True(t, snap.ComputedAt.IsZero(), "pre-publish snapshot has zero ComputedAt as 'never computed' marker")
	assert.Equal(t, 0.0, snap.TotalHeadroom)
}

func TestSupply_PublishThenLoad(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	want := &SupplySnapshot{
		ComputedAt:    now,
		TotalHeadroom: 5.0,
		Pressure:      0.6,
	}
	s.publishSupply(want)

	got := s.Supply()
	assert.Equal(t, want, got)
}

func TestSupply_ConcurrentLoadStore_RaceClean(t *testing.T) {
	s, _ := openTempSubsystem(t)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					s.publishSupply(&SupplySnapshot{ComputedAt: now, TotalHeadroom: float64(i)})
				}
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = s.Supply()
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	// Race detector catches violations; pass = no race reported.
}

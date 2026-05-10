package reputation

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/config"
)

type capturedFreeze struct {
	id        ids.IdentityID
	reason    string
	revokedAt time.Time
}

type fakeFreezeListener struct {
	mu    sync.Mutex
	calls []capturedFreeze
}

func (f *fakeFreezeListener) OnFreeze(_ context.Context, id ids.IdentityID, reason string, revokedAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, capturedFreeze{id, reason, revokedAt})
}

func (f *fakeFreezeListener) snapshot() []capturedFreeze {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]capturedFreeze, len(f.calls))
	copy(out, f.calls)
	return out
}

func openForListenerTest(t *testing.T, clk *frozenClock, opts ...Option) *Subsystem {
	t.Helper()
	cfg := config.DefaultConfig().Reputation
	cfg.StoragePath = filepath.Join(t.TempDir(), "rep.sqlite")
	cfg.MinPopulationForZScore = 5
	cfg.EvaluationIntervalS = 60
	cfg.ZScoreThreshold = 2.5
	all := append([]Option{WithClock(clk.Now)}, opts...)
	s, err := Open(context.Background(), cfg, all...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestFreezeListener_FiresOnAuditToFrozen(t *testing.T) {
	listener := &fakeFreezeListener{}
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	s := openForListenerTest(t, clk, WithFreezeListener(listener))

	id := mkID(0xF0)
	for i := 0; i < 3; i++ {
		require.NoError(t, s.RecordCategoricalBreach(id, BreachInvalidProofSignature))
		clk.Add(24 * time.Hour)
		require.NoError(t, s.runOneCycle(context.Background()))
	}

	require.Equal(t, StateFrozen, s.Status(id).State)
	calls := listener.snapshot()
	require.Len(t, calls, 1, "listener fires once on AUDIT->FROZEN")
	assert.Equal(t, id, calls[0].id)
	assert.Equal(t, "freeze_repeat", calls[0].reason)
	assert.Equal(t, clk.Now(), calls[0].revokedAt)
}

func TestFreezeListener_NotConfigured_NoCalls(t *testing.T) {
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	s := openForListenerTest(t, clk) // no listener

	id := mkID(0xF0)
	for i := 0; i < 3; i++ {
		require.NoError(t, s.RecordCategoricalBreach(id, BreachInvalidProofSignature))
		clk.Add(24 * time.Hour)
		require.NoError(t, s.runOneCycle(context.Background()))
	}
	require.Equal(t, StateFrozen, s.Status(id).State)
	// No listener — must not panic; nothing else to assert.
}

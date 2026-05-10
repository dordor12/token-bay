package reputation

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/tracker/internal/config"
)

// TestRace_ConcurrentIngestAndEvaluator hammers ingest from multiple
// goroutines while the evaluator runs cycles in parallel. The package
// is on the always-`-race` list (see tracker/CLAUDE.md); any race-
// detector hit here is a real bug.
func TestRace_ConcurrentIngestAndEvaluator(t *testing.T) {
	clk := &frozenClock{t: time.Unix(1_700_000_000, 0)}
	cfg := config.DefaultConfig().Reputation
	cfg.StoragePath = filepath.Join(t.TempDir(), "rep.sqlite")
	cfg.MinPopulationForZScore = 50
	s, err := Open(context.Background(), cfg, WithClock(clk.Now))
	require.NoError(t, err)
	defer s.Close()

	ctx := context.Background()
	var wg sync.WaitGroup

	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(seed byte) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				id := mkID(seed ^ byte(i))
				_ = s.RecordBrokerRequest(id, "admit")
				_ = s.RecordOfferOutcome(id, "accept")
				if i%17 == 0 {
					_ = s.RecordCategoricalBreach(id,
						BreachInvalidProofSignature)
				}
				_, _ = s.Score(id)
				_ = s.IsFrozen(id)
			}
		}(byte(w + 1))
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			_ = s.runOneCycle(ctx)
			time.Sleep(time.Millisecond)
		}
	}()

	wg.Wait()
}

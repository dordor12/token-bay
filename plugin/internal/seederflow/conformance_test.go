package seederflow_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/ccbridge"
	"github.com/token-bay/token-bay/plugin/internal/seederflow"
)

func TestRunConformance_SuccessKeepsAvailable(t *testing.T) {
	cfg := validConfig(t)
	cfg.ConformanceFn = func(context.Context, ccbridge.Runner) error { return nil }
	c, err := seederflow.New(cfg)
	require.NoError(t, err)
	require.NoError(t, c.RunConformance(context.Background()))
	require.True(t, c.Available(c.Now()))
}

func TestRunConformance_FailureSticksAndBlocksHandleOffer(t *testing.T) {
	cfg := validConfig(t)
	boom := errors.New("conformance: bash leak")
	cfg.ConformanceFn = func(context.Context, ccbridge.Runner) error { return boom }
	c, err := seederflow.New(cfg)
	require.NoError(t, err)
	err = c.RunConformance(context.Background())
	require.ErrorIs(t, err, boom)

	dec, err := c.HandleOffer(context.Background(), makeOffer("claude-sonnet-4-6"))
	require.NoError(t, err)
	require.False(t, dec.Accept)
	require.Contains(t, dec.RejectReason, "conformance")
}

func TestRunConformance_NilConformanceFnIsNoOp(t *testing.T) {
	cfg := validConfig(t)
	cfg.ConformanceFn = nil
	c, err := seederflow.New(cfg)
	require.NoError(t, err)
	require.NoError(t, c.RunConformance(context.Background()))
}

func TestRunConformance_RequiresRunnerWhenSet(t *testing.T) {
	cfg := validConfig(t)
	cfg.Runner = nil
	cfg.ConformanceFn = func(_ context.Context, r ccbridge.Runner) error {
		if r == nil {
			return errors.New("nil runner")
		}
		return nil
	}
	c, err := seederflow.New(cfg)
	require.NoError(t, err)
	require.Error(t, c.RunConformance(context.Background()))
}

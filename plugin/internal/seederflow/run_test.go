package seederflow_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/seederflow"
)

func TestRun_DispatchesAcceptedTunnelToServe(t *testing.T) {
	cfg := validConfig(t)
	bridge := &realStreamBridge{}
	cfg.Bridge = bridge
	tracker := &stubTracker{}
	cfg.Tracker = tracker
	acc := newStubAcceptor()
	cfg.Acceptor = acc
	c, err := seederflow.New(cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	// Send one offer.
	o := makeOffer("claude-sonnet-4-6")
	dec, err := c.HandleOffer(context.Background(), o)
	require.NoError(t, err)
	require.True(t, dec.Accept)

	// Push a fake tunnel into the acceptor.
	tn := &stubTunnel{body: makeAnthropicBody(t, "claude-sonnet-4-6", "hi")}
	acc.conns <- tn

	// Wait for UsageReport to land.
	require.Eventually(t, func() bool { return len(tracker.UsageReports()) == 1 }, 2*time.Second, 10*time.Millisecond)

	cancel()
	require.NoError(t, <-runDone)
}

func TestRun_AdvertisesAvailabilityOnStart(t *testing.T) {
	cfg := validConfig(t)
	tracker := &stubTracker{}
	cfg.Tracker = tracker
	acc := newStubAcceptor()
	cfg.Acceptor = acc
	c, err := seederflow.New(cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	require.Eventually(t, func() bool { return len(tracker.Advertisements()) >= 1 }, 2*time.Second, 10*time.Millisecond)
	require.True(t, tracker.Advertisements()[0].Available)
	require.Contains(t, tracker.Advertisements()[0].Models, "claude-sonnet-4-6")

	cancel()
	require.NoError(t, <-runDone)
}

func TestRun_RejectsSecondCall(t *testing.T) {
	cfg := validConfig(t)
	cfg.Acceptor = newStubAcceptor()
	c, err := seederflow.New(cfg)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- c.Run(ctx) }()

	// Give the goroutine a tick to mark started.
	time.Sleep(20 * time.Millisecond)
	require.ErrorIs(t, c.Run(ctx), seederflow.ErrAlreadyStarted)
	cancel()
	require.NoError(t, <-runDone)
}

package sidecar

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
)

func TestNew_HappyPath(t *testing.T) {
	deps := validDeps(t)
	app, err := New(deps)
	require.NoError(t, err)
	require.NotNil(t, app)
	require.NotNil(t, app.tracker)
	require.NotNil(t, app.proxy)
}

func TestNew_RejectsInvalidDeps(t *testing.T) {
	deps := validDeps(t)
	deps.Signer = nil
	app, err := New(deps)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidDeps))
	require.Nil(t, app)
}

func TestNew_AppliesTrackerClientOverrides(t *testing.T) {
	deps := validDeps(t)
	called := false
	deps.TrackerClientOverrides = func(cfg *trackerclient.Config) {
		called = true
		cfg.MaxFrameSize = 4096
	}
	_, err := New(deps)
	require.NoError(t, err)
	require.True(t, called, "override callback must be invoked")
}

func TestRun_ReturnsOnCtxCancel(t *testing.T) {
	deps := validDeps(t)
	app, err := New(deps)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- app.Run(ctx) }()

	// Give Run time to start subsystems.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of ctx cancel")
	}
}

func TestRun_RejectsSecondCall(t *testing.T) {
	deps := validDeps(t)
	app, err := New(deps)
	require.NoError(t, err)

	ctx := t.Context()
	go func() { _ = app.Run(ctx) }()
	require.Eventually(t, func() bool {
		app.startMu.Lock()
		defer app.startMu.Unlock()
		return app.started
	}, time.Second, 10*time.Millisecond)

	err = app.Run(ctx)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrAlreadyStarted))
}

func TestRun_RejectsAfterClose(t *testing.T) {
	deps := validDeps(t)
	app, err := New(deps)
	require.NoError(t, err)

	require.NoError(t, app.Close())

	err = app.Run(context.Background())
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrClosed))
}

func TestRun_CCProxyBindErrorPropagates(t *testing.T) {
	deps := validDeps(t)
	deps.CCProxyAddr = "127.0.0.1:1" // privileged port — bind fails for non-root
	app, err := New(deps)
	require.NoError(t, err)

	err = app.Run(t.Context())
	require.Error(t, err, "Run must return the bind error")
	require.Contains(t, err.Error(), "ccproxy")
}

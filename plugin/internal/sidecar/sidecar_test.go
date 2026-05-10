package sidecar

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/consumerflow"
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

// validConsumerFlowDeps returns the minimum set of consumerflow.Deps the
// orchestrator's Validate() requires. It intentionally uses a nil broker
// + audit log etc. only where consumerflow's *Validate* tolerates them —
// the wiring tests below never *invoke* the orchestrator, so those
// uncalled fields never see use.
func validConsumerFlowDeps(t *testing.T) consumerflow.Deps {
	t.Helper()
	return consumerflow.Deps{
		Tracker:         stubBroker{},
		Sessions:        stubSessions{},
		Settings:        stubSettings{},
		AuditLog:        stubAudit{},
		UsageProber:     stubProber{},
		ProofBuilder:    stubProofBuilder{},
		EnvelopeBuilder: stubEnvelopeBuilder{},
		Identity:        stubIdentity{},
		SidecarURLFunc:  func() string { return "http://127.0.0.1:0/" },
		DefaultModel:    "claude-sonnet-4-6",
		MaxInputTokens:  200_000,
		MaxOutputTokens: 8_192,
	}
}

// TestNew_WiresConsumerFlowAsSettlementHandler verifies that when
// Deps.ConsumerFlow is set, sidecar.New attaches it to
// trackerclient.Config.SettlementHandler — otherwise tracker-pushed
// settlement requests would be silently dropped.
func TestNew_WiresConsumerFlowAsSettlementHandler(t *testing.T) {
	deps := validDeps(t)
	cf, err := consumerflow.New(validConsumerFlowDeps(t))
	require.NoError(t, err)
	deps.ConsumerFlow = cf

	var observed trackerclient.SettlementHandler
	deps.TrackerClientOverrides = func(cfg *trackerclient.Config) {
		observed = cfg.SettlementHandler
	}

	_, err = New(deps)
	require.NoError(t, err)
	require.NotNil(t, observed, "SettlementHandler must be wired when ConsumerFlow is set")
}

// TestNew_NoConsumerFlow_LeavesSettlementHandlerNil mirrors the production
// invariant: a sidecar without consumer fallback must NOT register a
// settlement handler — trackerclient drops pushes silently in that case.
func TestNew_NoConsumerFlow_LeavesSettlementHandlerNil(t *testing.T) {
	deps := validDeps(t)
	var observed trackerclient.SettlementHandler
	deps.TrackerClientOverrides = func(cfg *trackerclient.Config) {
		observed = cfg.SettlementHandler
	}
	_, err := New(deps)
	require.NoError(t, err)
	require.Nil(t, observed, "SettlementHandler must remain nil when ConsumerFlow is unset")
}

func TestRun_CCProxyBindErrorPropagates(t *testing.T) {
	deps := validDeps(t)
	// Port 99999 is out of the 0-65535 range, so net.Listen rejects it
	// synchronously with a parse error on every platform — portable
	// alternative to the unix-only "privileged port" trick.
	deps.CCProxyAddr = "127.0.0.1:99999"
	app, err := New(deps)
	require.NoError(t, err)

	err = app.Run(t.Context())
	require.Error(t, err, "Run must return the bind error")
	require.Contains(t, err.Error(), "ccproxy")
}

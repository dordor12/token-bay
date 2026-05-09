package sidecar

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
)

func TestStatus_BeforeRun(t *testing.T) {
	deps := validDeps(t)
	app, err := New(deps)
	require.NoError(t, err)

	st := app.Status()
	require.False(t, st.Running)
	require.Equal(t, trackerclient.PhaseDisconnected, st.Tracker.Phase)
	require.Empty(t, st.CCProxyURL, "no resolved bind address before Start")
}

func TestStatus_AfterStart_HasResolvedCCProxyURL(t *testing.T) {
	deps := validDeps(t)
	app, err := New(deps)
	require.NoError(t, err)

	go func() { _ = app.Run(t.Context()) }()

	require.Eventually(t, func() bool {
		s := app.Status()
		return s.Running && s.CCProxyURL != ""
	}, 2*time.Second, 20*time.Millisecond)

	st := app.Status()
	require.False(t, st.StartedAt.IsZero(), "StartedAt must be stamped")
	require.Contains(t, st.CCProxyURL, "http://127.0.0.1:")
}

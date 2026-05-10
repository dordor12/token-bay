package seederflow_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/token-bay/token-bay/plugin/internal/seederflow"
)

func TestParseIdlePolicy_AlwaysOn(t *testing.T) {
	p, err := seederflow.ParseIdlePolicy("always_on", "")
	require.NoError(t, err)
	require.Equal(t, seederflow.IdleAlwaysOn, p.Mode)
}

func TestParseIdlePolicy_Scheduled(t *testing.T) {
	p, err := seederflow.ParseIdlePolicy("scheduled", "02:00-06:00")
	require.NoError(t, err)
	require.Equal(t, seederflow.IdleScheduled, p.Mode)
	require.Equal(t, 2, p.WindowStart.Hour())
	require.Equal(t, 6, p.WindowEnd.Hour())
}

func TestParseIdlePolicy_RejectsUnknownMode(t *testing.T) {
	_, err := seederflow.ParseIdlePolicy("turbo", "")
	require.Error(t, err)
}

func TestParseIdlePolicy_RejectsBadWindow(t *testing.T) {
	_, err := seederflow.ParseIdlePolicy("scheduled", "lol")
	require.Error(t, err)

	_, err = seederflow.ParseIdlePolicy("scheduled", "25:00-26:00")
	require.Error(t, err)
}

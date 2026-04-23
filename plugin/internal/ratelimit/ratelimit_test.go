package ratelimit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUsageVerdict_String(t *testing.T) {
	assert.Equal(t, "exhausted", UsageExhausted.String())
	assert.Equal(t, "headroom", UsageHeadroom.String())
	assert.Equal(t, "uncertain", UsageUncertain.String())
}

func TestGateVerdict_String(t *testing.T) {
	assert.Equal(t, "proceed", VerdictProceed.String())
	assert.Equal(t, "refuse-manual-ok", VerdictRefuseManualOK.String())
	assert.Equal(t, "refuse-with-reason", VerdictRefuseWithReason.String())
	assert.Equal(t, "skip-hook", VerdictSkipHook.String())
}

func TestErrorConstants_MatchSchemaValues(t *testing.T) {
	assert.Equal(t, "rate_limit", ErrorRateLimit)
	assert.Equal(t, "authentication_failed", ErrorAuthenticationFailed)
	assert.Equal(t, "billing_error", ErrorBillingError)
	assert.Equal(t, "invalid_request", ErrorInvalidRequest)
	assert.Equal(t, "server_error", ErrorServerError)
	assert.Equal(t, "max_output_tokens", ErrorMaxOutputTokens)
	assert.Equal(t, "unknown", ErrorUnknown)
}

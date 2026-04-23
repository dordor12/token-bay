package ratelimit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestApplyVerdictMatrix_HookRateLimitExhausted_Proceed(t *testing.T) {
	v := ApplyVerdictMatrix(TriggerHook, ErrorRateLimit, UsageExhausted)
	assert.Equal(t, VerdictProceed, v)
}

func TestApplyVerdictMatrix_HookRateLimitHeadroom_RefuseManualOK(t *testing.T) {
	v := ApplyVerdictMatrix(TriggerHook, ErrorRateLimit, UsageHeadroom)
	assert.Equal(t, VerdictRefuseManualOK, v)
}

func TestApplyVerdictMatrix_HookRateLimitUncertain_Proceed(t *testing.T) {
	v := ApplyVerdictMatrix(TriggerHook, ErrorRateLimit, UsageUncertain)
	assert.Equal(t, VerdictProceed, v)
}

func TestApplyVerdictMatrix_HookNonRateLimit_SkipHook(t *testing.T) {
	cases := []string{
		ErrorAuthenticationFailed, ErrorBillingError, ErrorServerError,
		ErrorInvalidRequest, ErrorMaxOutputTokens, ErrorUnknown, "anything-else",
	}
	for _, errName := range cases {
		t.Run(errName, func(t *testing.T) {
			v := ApplyVerdictMatrix(TriggerHook, errName, UsageExhausted)
			assert.Equal(t, VerdictSkipHook, v)
		})
	}
}

func TestApplyVerdictMatrix_ManualExhausted_Proceed(t *testing.T) {
	v := ApplyVerdictMatrix(TriggerManual, "", UsageExhausted)
	assert.Equal(t, VerdictProceed, v)
}

func TestApplyVerdictMatrix_ManualHeadroom_RefuseWithReason(t *testing.T) {
	v := ApplyVerdictMatrix(TriggerManual, "", UsageHeadroom)
	assert.Equal(t, VerdictRefuseWithReason, v)
}

func TestApplyVerdictMatrix_ManualUncertain_RefuseWithReason(t *testing.T) {
	v := ApplyVerdictMatrix(TriggerManual, "", UsageUncertain)
	assert.Equal(t, VerdictRefuseWithReason, v)
}

func TestCheckFreshness_AllFresh_ReturnsTrue(t *testing.T) {
	now := int64(1_000_000)
	assert.True(t, CheckFreshness(now-10, now-5, now))
}

func TestCheckFreshness_SignalsFarApart_ReturnsFalse(t *testing.T) {
	now := int64(1_000_000)
	assert.False(t, CheckFreshness(now-120, now, now))
}

func TestCheckFreshness_StopFailureStale_ReturnsFalse(t *testing.T) {
	now := int64(1_000_000)
	assert.False(t, CheckFreshness(now-200, now-10, now))
}

func TestCheckFreshness_UsageStale_ReturnsFalse(t *testing.T) {
	now := int64(1_000_000)
	assert.False(t, CheckFreshness(now-10, now-200, now))
}

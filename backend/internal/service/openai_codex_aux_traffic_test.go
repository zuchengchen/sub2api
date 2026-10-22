package service

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestCodexUsageSnapshotFromQuota(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	snap := codexUsageSnapshotFromQuota(&OpenAIQuotaUsage{RateLimit: &OpenAIRateLimit{
		PrimaryWindow:   &OpenAIRateLimitWindow{UsedPercent: 42.5, LimitWindowSeconds: 18000, ResetAfterSeconds: 600},
		SecondaryWindow: &OpenAIRateLimitWindow{UsedPercent: 7, LimitWindowSeconds: 604800, ResetAt: now.Unix() + 3600},
	}}, now)
	require.NotNil(t, snap)
	require.InDelta(t, 42.5, *snap.PrimaryUsedPercent, 0.001)
	require.Equal(t, 300, *snap.PrimaryWindowMinutes)
	require.Equal(t, 600, *snap.PrimaryResetAfterSeconds)
	require.Equal(t, 10080, *snap.SecondaryWindowMinutes)
	require.Equal(t, 3600, *snap.SecondaryResetAfterSeconds)
	require.NotEmpty(t, buildCodexUsageExtraUpdates(snap, now))
	require.Nil(t, codexUsageSnapshotFromQuota(&OpenAIQuotaUsage{}, now))
}

func TestShouldEnrichOpenAITokenOnRefresh_OncePerDay(t *testing.T) {
	info := &OpenAITokenInfo{ChatGPTAccountID: "acct-enrich-test", ChatGPTUserID: "user-enrich-test"}
	now := time.Now()
	require.True(t, shouldEnrichOpenAITokenOnRefresh(info, now))
	require.False(t, shouldEnrichOpenAITokenOnRefresh(info, now.Add(time.Hour)), "token refresh must not hit web endpoints every time")
	require.True(t, shouldEnrichOpenAITokenOnRefresh(info, now.Add(25*time.Hour)))
}

func TestOpenAITiboRoute_DisabledByShippedDefault(t *testing.T) {
	off := false
	cfg := &config.Config{}
	cfg.Gateway.OpenAITiboRoute.Enabled = &off
	svc := &OpenAIGatewayService{cfg: cfg}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "t"}}
	require.False(t, svc.openAITiboRouteApplies(account), "Tibo probes are synthetic traffic; off unless enabled")
}

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

func TestOpenAITiboRoute_DisabledByConfig(t *testing.T) {
	off := false
	cfg := &config.Config{}
	cfg.Gateway.OpenAITiboRoute.Enabled = &off
	svc := &OpenAIGatewayService{cfg: cfg}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "t"}}
	require.False(t, svc.openAITiboRouteApplies(account), "enabled=false turns Tibo probes off")
}

func TestCodexSandboxTagForOutboundPlatform_LinuxUA(t *testing.T) {
	// Canonical outbound UA is codex-tui on Ubuntu/x86_64.
	next, changed := codexSandboxTagForOutboundPlatform("seatbelt")
	require.True(t, changed)
	require.Equal(t, "seccomp", next)
	_, changed = codexSandboxTagForOutboundPlatform("seccomp")
	require.False(t, changed)
	_, changed = codexSandboxTagForOutboundPlatform("none")
	require.False(t, changed)
	next, changed = codexSandboxTagForOutboundPlatform("windows_sandbox")
	require.True(t, changed)
	require.Equal(t, "seccomp", next)
}

// The admin astra health dots read OpenAICodexTicketStatuses; with Tibo on
// they must carry an HTTP route row, and with it off the row disappears.
func TestOpenAICodexTicketStatuses_FollowTiboEnabled(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		tc := newTiboRouteCase(t, false, false)
		on := enabled
		tc.svc.cfg.Gateway.OpenAITiboRoute.Enabled = &on
		got := (&AccountTestService{openaiGatewayService: tc.svc}).OpenAICodexTicketStatuses(tc.account, config.OpenAICodexTicketConfig{}, time.Now())
		if !enabled {
			require.Nil(t, got)
			continue
		}
		require.Len(t, got, 1)
		require.Equal(t, openAICodexTicketDefaultModel, got[0].Model)
		require.Equal(t, "http", got[0].TiboRoutes[0].Route)
	}
}

// After a restart, or while an account is skipped (rate-limited/paused), the
// probe state is not seeded yet; the admin sparkline falls back to the
// persisted samples instead of rendering ten empty dots.
func TestOpenAITiboRouteStatuses_FallBackToPersistedSamples(t *testing.T) {
	tc := newTiboRouteCase(t, false, false)
	at := time.Now().Add(-time.Hour).UTC()
	tc.account.Extra[openAITiboVerdictExtraKey(openAITiboRouteHTTP)] = map[string]any{
		"model": openAICodexTicketDefaultModel,
		"recent_samples": []any{
			map[string]any{"verdict": "healthy", "at": at.Format(time.RFC3339Nano)},
			map[string]any{"verdict": "unknown", "at": at.Add(time.Minute).Format(time.RFC3339Nano)},
			map[string]any{"verdict": "degraded", "at": at.Add(2 * time.Minute).Format(time.RFC3339Nano)},
		},
	}
	status := tc.svc.openAITiboRouteStatuses(tc.account, time.Now())
	require.Len(t, status, 1)
	require.Len(t, status[0].RecentSamples, 2)
	require.Equal(t, "healthy", status[0].RecentSamples[0].Verdict)
	require.Equal(t, "degraded", status[0].RecentSamples[1].Verdict)
	require.Nil(t, tc.svc.loadOpenAITiboAccountState(tc.account.ID), "status view must not seed probe state")
}

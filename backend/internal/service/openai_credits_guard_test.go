package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newCreditsGuardTestAccount(id int64, extra map[string]any) Account {
	return Account{
		ID:          id,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Extra:       extra,
	}
}

func exhaustedWeeklySnapshot() *OpenAICodexUsageSnapshot {
	return &OpenAICodexUsageSnapshot{
		PrimaryUsedPercent:         ptrFloat64WS(100),
		PrimaryResetAfterSeconds:   ptrIntWS(3600),
		PrimaryWindowMinutes:       ptrIntWS(10080),
		SecondaryUsedPercent:       ptrFloat64WS(12),
		SecondaryResetAfterSeconds: ptrIntWS(1200),
		SecondaryWindowMinutes:     ptrIntWS(300),
	}
}

func TestAccount_IsOpenAICreditsGuardActive(t *testing.T) {
	oauth := newCreditsGuardTestAccount(1, nil)
	require.True(t, oauth.IsOpenAICreditsGuardActive(), "默认未启用点数时应受保护")

	enabled := newCreditsGuardTestAccount(2, map[string]any{OpenAICreditsEnabledExtraKey: true})
	require.False(t, enabled.IsOpenAICreditsGuardActive())

	enabledString := newCreditsGuardTestAccount(3, map[string]any{OpenAICreditsEnabledExtraKey: "true"})
	require.False(t, enabledString.IsOpenAICreditsGuardActive())

	setupToken := newCreditsGuardTestAccount(4, nil)
	setupToken.Type = AccountTypeSetupToken
	require.True(t, setupToken.IsOpenAICreditsGuardActive())

	apiKey := newCreditsGuardTestAccount(5, nil)
	apiKey.Type = AccountTypeAPIKey
	require.False(t, apiKey.IsOpenAICreditsGuardActive())

	parentID := int64(1)
	shadow := newCreditsGuardTestAccount(6, nil)
	shadow.ParentAccountID = &parentID
	require.False(t, shadow.IsOpenAICreditsGuardActive())
}

func TestOpenAICodexExhaustedResetAt(t *testing.T) {
	now := time.Now()

	_, _, ok := openAICodexExhaustedResetAt(&OpenAICodexUsageSnapshot{
		PrimaryUsedPercent:       ptrFloat64WS(99.9),
		PrimaryResetAfterSeconds: ptrIntWS(3600),
		PrimaryWindowMinutes:     ptrIntWS(300),
	}, now)
	require.False(t, ok, "未达 100% 不应判定用尽")

	resetAt, window, ok := openAICodexExhaustedResetAt(exhaustedWeeklySnapshot(), now)
	require.True(t, ok)
	require.Equal(t, "7d", window)
	require.WithinDuration(t, now.Add(time.Hour), resetAt, 2*time.Second)

	// 两个窗口都用尽时取较晚的重置时间
	both := &OpenAICodexUsageSnapshot{
		PrimaryUsedPercent:         ptrFloat64WS(100),
		PrimaryResetAfterSeconds:   ptrIntWS(600),
		PrimaryWindowMinutes:       ptrIntWS(300),
		SecondaryUsedPercent:       ptrFloat64WS(100),
		SecondaryResetAfterSeconds: ptrIntWS(7200),
		SecondaryWindowMinutes:     ptrIntWS(10080),
	}
	resetAt, window, ok = openAICodexExhaustedResetAt(both, now)
	require.True(t, ok)
	require.Equal(t, "7d", window)
	require.WithinDuration(t, now.Add(2*time.Hour), resetAt, 2*time.Second)

	_, _, ok = openAICodexExhaustedResetAt(&OpenAICodexUsageSnapshot{
		PrimaryUsedPercent:   ptrFloat64WS(100),
		PrimaryWindowMinutes: ptrIntWS(300),
	}, now)
	require.False(t, ok, "缺少重置倒计时无法确定限流时长")
}

func TestOpenAIGatewayService_UpdateCodexUsageSnapshot_CreditsGuardSetsRateLimit(t *testing.T) {
	repo := &openAICodexSnapshotAsyncRepo{
		stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{newCreditsGuardTestAccount(611, nil)}},
		updateExtraCh:         make(chan map[string]any, 1),
		rateLimitCh:           make(chan time.Time, 1),
	}
	svc := &OpenAIGatewayService{accountRepo: repo}
	svc.updateCodexUsageSnapshot(context.Background(), 611, exhaustedWeeklySnapshot())

	select {
	case resetAt := <-repo.rateLimitCh:
		require.WithinDuration(t, time.Now().Add(time.Hour), resetAt, 5*time.Second)
	case <-time.After(2 * time.Second):
		t.Fatal("未启用点数的账号额度用尽后应被限流")
	}
}

func TestOpenAIGatewayService_UpdateCodexUsageSnapshot_CreditsEnabledDoesNotSetRateLimit(t *testing.T) {
	repo := &openAICodexSnapshotAsyncRepo{
		stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{
			newCreditsGuardTestAccount(612, map[string]any{OpenAICreditsEnabledExtraKey: true}),
		}},
		updateExtraCh: make(chan map[string]any, 1),
		rateLimitCh:   make(chan time.Time, 1),
	}
	svc := &OpenAIGatewayService{accountRepo: repo}
	svc.updateCodexUsageSnapshot(context.Background(), 612, exhaustedWeeklySnapshot())

	select {
	case <-repo.updateExtraCh:
	case <-time.After(2 * time.Second):
		t.Fatal("等待 codex 快照落库超时")
	}
	select {
	case resetAt := <-repo.rateLimitCh:
		t.Fatalf("已启用点数的账号不应被限流: %v", resetAt)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestOpenAIGatewayService_UpdateCodexUsageSnapshot_CreditsGuardBypassesSnapshotThrottle(t *testing.T) {
	repo := &openAICodexSnapshotAsyncRepo{
		stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{newCreditsGuardTestAccount(613, nil)}},
		updateExtraCh:         make(chan map[string]any, 2),
		rateLimitCh:           make(chan time.Time, 1),
	}
	svc := &OpenAIGatewayService{accountRepo: repo, codexSnapshotThrottle: newAccountWriteThrottle(time.Hour)}
	normal := &OpenAICodexUsageSnapshot{
		PrimaryUsedPercent:       ptrFloat64WS(80),
		PrimaryResetAfterSeconds: ptrIntWS(3600),
		PrimaryWindowMinutes:     ptrIntWS(10080),
	}
	svc.updateCodexUsageSnapshot(context.Background(), 613, normal)
	svc.updateCodexUsageSnapshot(context.Background(), 613, exhaustedWeeklySnapshot())

	for i := 0; i < 2; i++ {
		select {
		case <-repo.updateExtraCh:
		case <-time.After(2 * time.Second):
			t.Fatalf("用尽快照应绕过节流落库，第 %d 次写入超时", i+1)
		}
	}
}

func TestShouldAutoPauseOpenAIAccountByQuota_CreditsGuard(t *testing.T) {
	resetAt := time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339)
	updatedAt := time.Now().UTC().Format(time.RFC3339)
	exhaustedExtra := func(extra map[string]any) map[string]any {
		base := map[string]any{
			"codex_7d_used_percent":  100.0,
			"codex_7d_reset_at":      resetAt,
			"codex_usage_updated_at": updatedAt,
			// 即使关闭了提前暂停，点数保护仍需生效
			"auto_pause_7d_disabled": true,
		}
		for k, v := range extra {
			base[k] = v
		}
		return base
	}

	guarded := newCreditsGuardTestAccount(801, exhaustedExtra(nil))
	paused, decision := shouldAutoPauseOpenAIAccountByQuota(context.Background(), &guarded)
	require.True(t, paused)
	require.Equal(t, "credits_guard_exhausted_7d", decision.reason)
	require.Equal(t, "credits_guard_exhausted_7d",
		openAICompatibleAccountEligibilityFailureReasonBeforeProfit(context.Background(), &guarded, PlatformOpenAI, "", false, ""))

	enabled := newCreditsGuardTestAccount(802, exhaustedExtra(map[string]any{OpenAICreditsEnabledExtraKey: true}))
	paused, _ = shouldAutoPauseOpenAIAccountByQuota(context.Background(), &enabled)
	require.False(t, paused, "已启用点数的账号应继续调度")

	notExhausted := newCreditsGuardTestAccount(803, exhaustedExtra(map[string]any{"codex_7d_used_percent": 99.0}))
	paused, _ = shouldAutoPauseOpenAIAccountByQuota(context.Background(), &notExhausted)
	require.False(t, paused)

	windowReset := newCreditsGuardTestAccount(804, exhaustedExtra(map[string]any{
		"codex_7d_reset_at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	}))
	paused, _ = shouldAutoPauseOpenAIAccountByQuota(context.Background(), &windowReset)
	require.False(t, paused, "窗口重置后应自动放行")
}

package service

import (
	"context"
	"log/slog"
	"time"
)

// openAICreditsGuardCheckMinInterval 限制同一账号在额度用尽后重复回查 DB 的频率。
// 首次命中后账号会被持久化限流并从调度中移除；该节流只吸收已在途的并发响应。
const openAICreditsGuardCheckMinInterval = 5 * time.Second

// openAICreditsGuardRateLimitReason 是点数保护写入运行时调度阻断时使用的原因。
const openAICreditsGuardRateLimitReason = "credits_guard_exhausted"

var openAICreditsGuardCheckThrottle = newAccountWriteThrottle(openAICreditsGuardCheckMinInterval)

// openAICodexExhaustedResetAt 在任一 Codex 窗口 used_percent >= 100 时返回应限流到的时间。
// 两个窗口同时用尽时取较晚的重置时间（两者都重置后套餐额度才真正可用）。
// 用尽但缺少重置倒计时的窗口无法确定限流时长，交由调度层快照守卫处理。
func openAICodexExhaustedResetAt(snapshot *OpenAICodexUsageSnapshot, now time.Time) (time.Time, string, bool) {
	if snapshot == nil {
		return time.Time{}, "", false
	}
	normalized := snapshot.Normalize()
	if normalized == nil {
		return time.Time{}, "", false
	}
	base := codexSnapshotBaseTime(snapshot, now)
	var (
		resetAt time.Time
		window  string
	)
	consider := func(used *float64, resetAfter *int, name string) {
		if used == nil || *used < 100 || resetAfter == nil {
			return
		}
		sec := *resetAfter
		if sec < 0 {
			sec = 0
		}
		candidate := base.Add(time.Duration(sec) * time.Second)
		if !candidate.After(now) {
			return
		}
		if candidate.After(resetAt) {
			resetAt = candidate
			window = name
		}
	}
	consider(normalized.Used5hPercent, normalized.Reset5hSeconds, "5h")
	consider(normalized.Used7dPercent, normalized.Reset7dSeconds, "7d")
	if window == "" {
		return time.Time{}, "", false
	}
	return resetAt, window, true
}

// applyOpenAICreditsGuard 在成功响应头显示套餐额度已用尽时，把未启用点数的账号
// 持久化为 429 限流直到窗口重置，阻止后续请求让上游自动消耗购买的点数。
// 已启用点数（管理员手动确认）的账号不受影响。
// 返回 true 表示本次快照已用尽且通过了节流（调用方据此绕过快照写入节流）。
func (s *OpenAIGatewayService) applyOpenAICreditsGuard(ctx context.Context, accountID int64, snapshot *OpenAICodexUsageSnapshot) bool {
	if s == nil || s.accountRepo == nil || accountID <= 0 {
		return false
	}
	now := time.Now()
	resetAt, window, exhausted := openAICodexExhaustedResetAt(snapshot, now)
	if !exhausted {
		return false
	}
	if !openAICreditsGuardCheckThrottle.Allow(accountID, now) {
		return false
	}
	go s.persistOpenAICreditsGuardRateLimit(ctx, accountID, resetAt, window)
	return true
}

func (s *OpenAIGatewayService) persistOpenAICreditsGuardRateLimit(ctx context.Context, accountID int64, resetAt time.Time, window string) {
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()

	account, err := s.accountRepo.GetByID(stateCtx, accountID)
	if err != nil || account == nil {
		return
	}
	if !account.IsOpenAICreditsGuardActive() {
		return
	}
	if account.RateLimitResetAt != nil && !account.RateLimitResetAt.Before(resetAt) {
		return
	}

	s.BlockAccountScheduling(account, resetAt, openAICreditsGuardRateLimitReason)
	if extendingRepo, ok := s.accountRepo.(grokRateLimitExtendingRepository); ok {
		err = extendingRepo.SetRateLimitedIfLater(stateCtx, account.ID, resetAt)
	} else {
		err = s.accountRepo.SetRateLimited(stateCtx, account.ID, resetAt)
	}
	if err != nil {
		slog.Warn("openai_credits_guard_rate_limit_failed",
			"account_id", account.ID,
			"window", window,
			"reset_at", resetAt.UTC(),
			"error", err,
		)
		return
	}
	slog.Info("openai_credits_guard_rate_limited",
		"account_id", account.ID,
		"window", window,
		"reset_at", resetAt.UTC(),
	)
}

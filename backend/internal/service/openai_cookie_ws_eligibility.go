package service

import (
	"context"
	"errors"
	"maps"
	"time"
)

var errOpenAICookieWSAccountUnavailable = errors.New("cookie websocket account is unavailable")

// Cookie acquisition and replenishment obey the same stop conditions as
// business scheduling. In contrast, the retired 292 harvester intentionally
// ignored rate-limit/overload cooldowns; never reuse its eligibility policy here.
func openAICookieWSAccountSkipReason(account *Account, now time.Time) string {
	if account == nil || account.ID <= 0 || !isOpenAICodexTicketAccount(account) || account.Type != AccountTypeOAuth || account.IsOpenAIAgentIdentity() {
		return "not_cookie_account"
	}
	if account.Status != StatusActive {
		return "not_active"
	}
	if !account.Schedulable {
		return "manual_unschedulable"
	}
	if account.AutoPauseOnExpired && account.ExpiresAt != nil && !now.Before(*account.ExpiresAt) {
		return "expired"
	}
	if account.RateLimitResetAt != nil && now.Before(*account.RateLimitResetAt) {
		return "rate_limited"
	}
	if account.OverloadUntil != nil && now.Before(*account.OverloadUntil) {
		return "overloaded"
	}
	if account.TempUnschedulableUntil != nil && now.Before(*account.TempUnschedulableUntil) {
		return "temporarily_unschedulable"
	}
	// The probe uses the actual Astra model, not the account's client mapping.
	if reset := account.modelRateLimitResetAt(openAICodexTicketDefaultModel); reset != nil && now.Before(*reset) {
		return "model_rate_limited"
	}
	if window := openAICodexTicketHarvestQuotaExhaustedWindow(account, now); window != "" {
		return "quota_" + window
	}
	return ""
}

// Every new upstream request must consult the authoritative account, not the
// list snapshot retained by a background pass. Repository errors fail closed.
// The cloned snapshot can safely be enriched by token/quota helpers without
// mutating repository or scheduler-owned maps.
func (s *OpenAIGatewayService) latestOpenAICookieWSAccount(ctx context.Context, accountID int64) (*Account, error) {
	if s == nil || s.accountRepo == nil || accountID <= 0 {
		return nil, errOpenAICookieWSAccountUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	account, err := s.accountRepo.GetByID(readCtx, accountID)
	if err != nil || account == nil || account.ID != accountID {
		return nil, errOpenAICookieWSAccountUnavailable
	}
	current := *account
	current.Extra = maps.Clone(account.Extra)
	current.Credentials = maps.Clone(account.Credentials)
	if !s.openAICookieWSAccountEnabled(&current) || openAICookieWSAccountSkipReason(&current, time.Now()) != "" {
		return nil, errOpenAICookieWSAccountUnavailable
	}
	if s.isOpenAIAccountRuntimeBlocked(&current) || s.getOpenAIAccountModelTransientState().isBlocked(current.ID, openAIAccountModelTransientModel(openAICodexTicketDefaultModel), time.Now()) {
		return nil, errOpenAICookieWSAccountUnavailable
	}
	return &current, nil
}

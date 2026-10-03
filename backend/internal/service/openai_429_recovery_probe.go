package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	httppool "github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

const (
	openai429RecoveryMaxConcurrency = 4
	openai429RecoveryProbeTimeout   = 20 * time.Second
	openai429RecoveryMinRemaining   = 30 * time.Second
)

var openai429RecoveryBackoff = []time.Duration{
	30 * time.Second,
	time.Minute,
	2 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
}

type openai429RecoveryState struct {
	timer         *time.Timer
	attempts      int
	model         string
	cooldownUntil time.Time
	inFlight      bool
}

type openai429RecoveryProbeResult struct {
	Status  int
	Headers http.Header
	Stop    bool
	Err     error
}

type openai429RecoveryProber func(ctx context.Context, account *Account, model string) openai429RecoveryProbeResult

func (s *RateLimitService) markOpenAIFormalCooldown(account *Account, resetAt time.Time, requestedModel ...string) {
	if s == nil || account == nil || !account.IsOpenAI() || account.IsShadow() {
		return
	}
	now := time.Now()
	account.RateLimitedAt = &now
	reset := resetAt
	account.RateLimitResetAt = &reset
	s.scheduleOpenAI429RecoveryProbe(account, true, requestedModel...)
}

func (s *RateLimitService) ConsiderOpenAI429Recovery(account *Account) {
	s.scheduleOpenAI429RecoveryProbe(account, false)
}

func (s *RateLimitService) scheduleOpenAI429RecoveryProbe(account *Account, resetAttempt bool, requestedModel ...string) {
	if s == nil || account == nil || account.ID <= 0 {
		return
	}
	now := time.Now()
	if !openAIAccountEligibleFor429RecoveryProbe(account, now) {
		s.cancelOpenAI429RecoveryProbe(account.ID)
		return
	}

	model := openai429RecoveryProbeModel(account, firstRequestedModel(requestedModel))
	s.openai429RecoveryMu.Lock()
	defer s.openai429RecoveryMu.Unlock()
	if s.openai429RecoveryStates == nil {
		s.openai429RecoveryStates = map[int64]*openai429RecoveryState{}
	}

	state := s.openai429RecoveryStates[account.ID]
	if state != nil {
		state.cooldownUntil = *account.RateLimitResetAt
		if model != "" {
			state.model = model
		}
		if state.inFlight || (!resetAttempt && state.timer != nil) {
			return
		}
		if state.timer != nil {
			state.timer.Stop()
			state.timer = nil
		}
		if resetAttempt {
			state.attempts = 0
		}
	} else {
		state = &openai429RecoveryState{
			model:         model,
			cooldownUntil: *account.RateLimitResetAt,
		}
		s.openai429RecoveryStates[account.ID] = state
	}

	delay := openAI429RecoveryDelay(state.attempts)
	if remaining := time.Until(state.cooldownUntil); remaining <= delay {
		delete(s.openai429RecoveryStates, account.ID)
		return
	}
	accountID := account.ID
	state.timer = s.afterOpenAI429Recovery(delay, func() {
		s.fireOpenAI429RecoveryProbe(accountID)
	})
	slog.Info("openai_429_recovery_probe_scheduled",
		"account_id", account.ID,
		"delay", delay,
		"attempts", state.attempts,
		"model", state.model,
		"cooldown_until", state.cooldownUntil,
		"reset_attempt", resetAttempt,
	)
}

func (s *RateLimitService) cancelOpenAI429RecoveryProbe(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	s.openai429RecoveryMu.Lock()
	defer s.openai429RecoveryMu.Unlock()
	state := s.openai429RecoveryStates[accountID]
	if state == nil {
		return
	}
	if state.timer != nil {
		state.timer.Stop()
	}
	delete(s.openai429RecoveryStates, accountID)
}

func (s *RateLimitService) afterOpenAI429Recovery(d time.Duration, fn func()) *time.Timer {
	if s != nil && s.openai429RecoveryAfter != nil {
		return s.openai429RecoveryAfter(d, fn)
	}
	return time.AfterFunc(d, fn)
}

func (s *RateLimitService) fireOpenAI429RecoveryProbe(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	if s.openai429RecoverySlots != nil {
		select {
		case s.openai429RecoverySlots <- struct{}{}:
			defer func() { <-s.openai429RecoverySlots }()
		default:
			s.rescheduleOpenAI429RecoveryProbe(accountID)
			return
		}
	}

	s.openai429RecoveryMu.Lock()
	state := s.openai429RecoveryStates[accountID]
	if state == nil {
		s.openai429RecoveryMu.Unlock()
		return
	}
	if state.timer != nil {
		state.timer.Stop()
		state.timer = nil
	}
	state.inFlight = true
	model := state.model
	s.openai429RecoveryMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), openai429RecoveryProbeTimeout)
	defer cancel()

	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil || account == nil || !openAIAccountEligibleFor429RecoveryProbe(account, time.Now()) {
		s.cancelOpenAI429RecoveryProbe(accountID)
		slog.Info("openai_429_recovery_probe_stopped", "account_id", accountID, "reason", "ineligible")
		return
	}
	if model == "" {
		model = openai429RecoveryProbeModel(account, "")
	}

	result := s.runOpenAI429RecoveryProbe(ctx, account, model)
	if result.Headers != nil {
		s.persistOpenAICodexSnapshot(ctx, account, result.Headers)
	}
	if result.Stop {
		s.cancelOpenAI429RecoveryProbe(accountID)
		slog.Info("openai_429_recovery_probe_stopped", "account_id", accountID, "reason", "probe_stop", "error", result.Err)
		return
	}
	if result.Err != nil {
		s.retryOpenAI429RecoveryProbe(accountID)
		slog.Info("openai_429_recovery_probe_retry", "account_id", accountID, "reason", "probe_error", "error", result.Err)
		return
	}
	if result.Status >= 200 && result.Status < 300 {
		latest := account
		if reloaded, loadErr := s.accountRepo.GetByID(ctx, accountID); loadErr == nil && reloaded != nil {
			latest = reloaded
			if result.Headers != nil {
				mergeAccountExtra(latest, buildCodexUsageExtraUpdates(ParseCodexRateLimitHeaders(result.Headers), time.Now()))
			}
		}
		if openAIAccountEligibleFor429RecoveryProbe(latest, time.Now()) {
			s.recoverAfterOpenAI429ProbeSuccess(ctx, accountID, model)
			slog.Info("openai_429_recovery_probe_success", "account_id", accountID, "model", model, "status", result.Status)
		} else {
			s.cancelOpenAI429RecoveryProbe(accountID)
			slog.Info("openai_429_recovery_probe_stopped", "account_id", accountID, "reason", "ineligible_after_success")
		}
		return
	}
	if result.Status == http.StatusTooManyRequests {
		latest := account
		if reloaded, loadErr := s.accountRepo.GetByID(ctx, accountID); loadErr == nil && reloaded != nil {
			latest = reloaded
		}
		if !openAIQuotaWindowsAllow429RecoveryProbe(latest.Extra, time.Now()) {
			s.cancelOpenAI429RecoveryProbe(accountID)
			slog.Info("openai_429_recovery_probe_stopped", "account_id", accountID, "reason", "windows_exhausted")
			return
		}
		s.retryOpenAI429RecoveryProbe(accountID)
		slog.Info("openai_429_recovery_probe_retry", "account_id", accountID, "reason", "upstream_429", "status", result.Status)
		return
	}
	if result.Status == http.StatusUnauthorized || result.Status == http.StatusForbidden {
		s.cancelOpenAI429RecoveryProbe(accountID)
		slog.Info("openai_429_recovery_probe_stopped", "account_id", accountID, "reason", "auth", "status", result.Status)
		return
	}
	s.retryOpenAI429RecoveryProbe(accountID)
	slog.Info("openai_429_recovery_probe_retry", "account_id", accountID, "reason", "upstream_status", "status", result.Status)
}

func (s *RateLimitService) runOpenAI429RecoveryProbe(ctx context.Context, account *Account, model string) openai429RecoveryProbeResult {
	if s != nil && s.openai429RecoveryProbe != nil {
		return s.openai429RecoveryProbe(ctx, account, model)
	}
	return s.probeOpenAI429RecoveryHTTP(ctx, account, model)
}

func (s *RateLimitService) retryOpenAI429RecoveryProbe(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	s.openai429RecoveryMu.Lock()
	defer s.openai429RecoveryMu.Unlock()
	state := s.openai429RecoveryStates[accountID]
	if state == nil {
		return
	}
	state.inFlight = false
	state.attempts++
	if state.timer != nil {
		state.timer.Stop()
		state.timer = nil
	}
	delay := openAI429RecoveryDelay(state.attempts)
	if remaining := time.Until(state.cooldownUntil); remaining <= delay {
		delete(s.openai429RecoveryStates, accountID)
		return
	}
	state.timer = s.afterOpenAI429Recovery(delay, func() {
		s.fireOpenAI429RecoveryProbe(accountID)
	})
}

func (s *RateLimitService) rescheduleOpenAI429RecoveryProbe(accountID int64) {
	if s == nil || accountID <= 0 {
		return
	}
	s.openai429RecoveryMu.Lock()
	defer s.openai429RecoveryMu.Unlock()
	state := s.openai429RecoveryStates[accountID]
	if state == nil || state.inFlight {
		return
	}
	if state.timer != nil {
		state.timer.Stop()
	}
	delay := openAI429RecoveryDelay(state.attempts)
	state.timer = s.afterOpenAI429Recovery(delay, func() {
		s.fireOpenAI429RecoveryProbe(accountID)
	})
}

func (s *RateLimitService) recoverAfterOpenAI429ProbeSuccess(ctx context.Context, accountID int64, model string) {
	if s == nil || s.accountRepo == nil || accountID <= 0 {
		return
	}
	if err := s.accountRepo.ClearRateLimit(ctx, accountID); err != nil {
		slog.Warn("openai_429_recovery_clear_rate_limit_failed", "account_id", accountID, "error", err)
		return
	}
	ResetOpenAI429Counter(accountID)
	s.notifyAccountSchedulingBlockCleared(accountID)
	s.clearOpenAI429RecoveryModelLimit(ctx, accountID, model)
	s.cancelOpenAI429RecoveryProbe(accountID)
}

func (s *RateLimitService) clearOpenAI429RecoveryModelLimit(ctx context.Context, accountID int64, model string) {
	if s == nil || s.accountRepo == nil || accountID <= 0 || strings.TrimSpace(model) == "" {
		return
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil || account == nil {
		return
	}
	extra := cloneExtraJSON(account.Extra)
	if extra == nil {
		return
	}
	rawLimits, ok := extra[modelRateLimitsKey].(map[string]any)
	if !ok || len(rawLimits) == 0 {
		return
	}
	changed := false
	for _, key := range account.modelRateLimitKeysForRequest(ctx, model) {
		if _, exists := rawLimits[key]; exists {
			delete(rawLimits, key)
			changed = true
		}
	}
	if !changed {
		return
	}
	if err := s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{modelRateLimitsKey: rawLimits}); err != nil {
		slog.Warn("openai_429_recovery_clear_model_limit_failed", "account_id", accountID, "model", model, "error", err)
	}
}

func (s *RateLimitService) probeOpenAI429RecoveryHTTP(ctx context.Context, account *Account, model string) openai429RecoveryProbeResult {
	if account == nil || !account.IsOAuth() {
		return openai429RecoveryProbeResult{Stop: true, Err: fmt.Errorf("unsupported account type")}
	}
	if account.IsOpenAIAgentIdentity() {
		return openai429RecoveryProbeResult{Stop: true, Err: fmt.Errorf("agent identity probe unsupported")}
	}
	authToken := account.GetOpenAIAccessToken()
	if authToken == "" {
		return openai429RecoveryProbeResult{Stop: true, Err: fmt.Errorf("no access token available")}
	}
	payload := createOpenAITestPayload(model, true)
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return openai429RecoveryProbeResult{Err: fmt.Errorf("marshal openai recovery probe payload: %w", err)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatgptCodexURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return openai429RecoveryProbeResult{Err: fmt.Errorf("create openai recovery probe request: %w", err)}
	}
	req.Host = "chatgpt.com"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+authToken)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	canonical := resolveCodexOutboundIdentity("")
	req.Header.Set("Originator", canonical.originator)
	req.Header.Set("Version", canonical.version)
	req.Header.Set("User-Agent", canonical.userAgent)
	enforceCodexIdentityHeadersWithUA(req.Header, account.GetOpenAIUserAgent())
	setOpenAIChatGPTAccountHeaders(req.Header, account)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	client, err := httppool.GetClient(httppool.Options{
		ProxyURL:              proxyURL,
		Timeout:               openai429RecoveryProbeTimeout,
		ResponseHeaderTimeout: 10 * time.Second,
	})
	if err != nil {
		return openai429RecoveryProbeResult{Err: fmt.Errorf("build openai recovery probe client: %w", err)}
	}
	resp, err := client.Do(req)
	if err != nil {
		return openai429RecoveryProbeResult{Err: fmt.Errorf("openai recovery probe request failed: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return openai429RecoveryProbeResult{Status: resp.StatusCode, Headers: resp.Header.Clone()}
}

func openAI429RecoveryDelay(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	if attempts >= len(openai429RecoveryBackoff) {
		return openai429RecoveryBackoff[len(openai429RecoveryBackoff)-1]
	}
	return openai429RecoveryBackoff[attempts]
}

func openai429RecoveryProbeModel(account *Account, requestedModel string) string {
	model := strings.TrimSpace(requestedModel)
	if account != nil {
		if mapped := strings.TrimSpace(account.GetMappedModel(model)); mapped != "" {
			model = mapped
		}
	}
	if model == "" {
		return openai.DefaultTestModel
	}
	return model
}

func openAIAccountEligibleFor429RecoveryProbe(account *Account, now time.Time) bool {
	if account == nil || !account.IsOpenAI() || account.IsShadow() {
		return false
	}
	if account.IsPoolMode() && !account.IsCustomErrorCodesEnabled() {
		return false
	}
	if account.RateLimitResetAt == nil || !now.Before(*account.RateLimitResetAt) {
		return false
	}
	if account.RateLimitResetAt.Sub(now) <= openai429RecoveryMinRemaining {
		return false
	}
	return openAIQuotaWindowsAllow429RecoveryProbe(account.Extra, now)
}

func openAIQuotaWindowsAllow429RecoveryProbe(extra map[string]any, now time.Time) bool {
	if !openAICodexSnapshotFreshForRecovery(extra, now) {
		return false
	}
	return openAIQuotaWindowHasRemaining(extra, "5h", now) && openAIQuotaWindowHasRemaining(extra, "7d", now)
}

func openAICodexSnapshotFreshForRecovery(extra map[string]any, now time.Time) bool {
	if len(extra) == 0 {
		return false
	}
	updatedRaw, ok := extra["codex_usage_updated_at"]
	if !ok {
		return false
	}
	updatedAt, err := parseTime(fmt.Sprint(updatedRaw))
	if err != nil {
		return false
	}
	return now.Sub(updatedAt) < openAICodexAutoPauseStaleAfter
}

func openAIQuotaWindowHasRemaining(extra map[string]any, window string, now time.Time) bool {
	if openAIQuotaWindowReset(extra, window, now) {
		return true
	}
	usedPercent, ok := resolveAccountExtraNumber(extra, "codex_"+window+"_used_percent")
	if !ok {
		return false
	}
	return usedPercent < 100
}

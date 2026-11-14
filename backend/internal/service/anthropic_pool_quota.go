package service

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// A relay that has exhausted its whole credential pool cannot recover by
// selecting another credential on the next immediate retry. Without an
// authoritative reset time, periodically allow it to be probed again instead
// of disabling the account until an assumed billing date.
const anthropicPoolQuotaProbeCooldown = 5 * time.Minute

func isAnthropicPoolQuotaExhausted(account *Account, statusCode int, body []byte) bool {
	if account == nil || account.Platform != PlatformAnthropic || account.Type != AccountTypeAPIKey ||
		!account.IsPoolMode() || statusCode != http.StatusTooManyRequests {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "error.code").String()), "all_credentials_quota_exhausted") {
		return true
	}
	// Match only the actual error message, never echoed request fields. A
	// generic insufficient_quota/rate_limit_error may concern just one member
	// of a pool, so it deliberately keeps the existing pool retry policy.
	message := strings.TrimSpace(extractUpstreamErrorMessage(body))
	return strings.HasPrefix(message, "所有可用凭据均已用尽订阅额度或超额额度") ||
		strings.HasPrefix(strings.ToLower(message), "all available credentials have exhausted their subscription or overage quota")
}

func (s *GatewayService) shouldRetryUpstreamResponse(account *Account, resp *http.Response) bool {
	if !s.shouldRetryUpstreamError(account, resp.StatusCode) {
		return false
	}
	if account.Platform != PlatformAnthropic || !account.IsPoolMode() || resp.StatusCode != http.StatusTooManyRequests {
		return true
	}
	body, readErr := s.readUpstreamErrorBody(resp)
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return readErr != nil || !isAnthropicPoolQuotaExhausted(account, resp.StatusCode, body)
}

func (s *RateLimitService) handleAnthropicPoolQuotaExhausted(ctx context.Context, account *Account, headers http.Header) {
	now := time.Now()
	var resetAt time.Time
	if reset := parseRetryAfterResetTime(headers, now); reset != nil && reset.After(now) {
		resetAt = *reset
	}
	if reset := calculateAnthropic429ResetTime(headers); reset != nil && reset.resetAt.After(now) && reset.resetAt.After(resetAt) {
		resetAt = reset.resetAt
	}
	if unix, err := strconv.ParseInt(strings.TrimSpace(headers.Get("anthropic-ratelimit-unified-reset")), 10, 64); err == nil {
		if reset := time.Unix(unix, 0); reset.After(now) && reset.After(resetAt) {
			resetAt = reset
		}
	}
	if resetAt.IsZero() {
		resetAt = now.Add(anthropicPoolQuotaProbeCooldown)
	}
	if account.RateLimitResetAt != nil && account.RateLimitResetAt.After(resetAt) {
		resetAt = *account.RateLimitResetAt
	}
	s.notifyAccountSchedulingBlocked(account, resetAt, "anthropic_pool_quota_exhausted")
	if s.accountRepo != nil {
		if err := s.accountRepo.SetRateLimited(ctx, account.ID, resetAt); err != nil {
			slog.Warn("anthropic_pool_quota_cooldown_failed", "account_id", account.ID, "error", err)
			return
		}
	}
	slog.Warn("anthropic_pool_quota_exhausted", "account_id", account.ID, "reset_at", resetAt)
}

// Called only for HTTP errors before any upstream response content has been
// forwarded. Response headers are retained for final error handling as well as
// an upstream-directed, bounded same-account retry delay.
func newGatewayHTTPFailoverError(account *Account, resp *http.Response, body []byte, shouldDisable bool) *UpstreamFailoverError {
	failover := &UpstreamFailoverError{
		StatusCode:             resp.StatusCode,
		ResponseHeaders:        resp.Header,
		ResponseBody:           body,
		RetryableOnSameAccount: !shouldDisable && account != nil && account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode) && !isAnthropicPoolQuotaExhausted(account, resp.StatusCode, body),
	}
	if failover.RetryableOnSameAccount && resp.StatusCode == http.StatusTooManyRequests {
		now := time.Now()
		if resetAt := parseRetryAfterResetTime(resp.Header, now); resetAt != nil && resetAt.After(now) {
			delay := resetAt.Sub(now)
			if delay > maxRetryElapsed {
				// Do not hold an inbound request for minutes or retry earlier than
				// requested by the relay. The handler may select another account.
				failover.RetryableOnSameAccount = false
			} else {
				failover.SameAccountRetryDelay = delay
			}
		}
	}
	return failover
}

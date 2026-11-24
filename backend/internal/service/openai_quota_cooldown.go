package service

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/tidwall/gjson"
)

// OpenAIQuotaCooldownExtraKey identifies a provider-confirmed quota deadline.
// The regular rate-limit fields remain the durable scheduling gates.
const OpenAIQuotaCooldownExtraKey = "openai_usage_limit_until"
const OpenAIQuotaProvisionalExtraKey = "openai_usage_limit_provisional"
const OpenAIQuotaExhaustedReason GatewayFailureReason = "usage_limit_reached"
const openAIQuotaRefreshInterval = time.Minute

type openAIQuotaCooldownRepository interface {
	ClearOpenAIRateLimitIfObserved(ctx context.Context, account *Account, clearOverload bool) (bool, error)
	// expected, when set, is the rate_limited_at generation seen before a quota lookup.
	SetOpenAIQuotaCooldown(ctx context.Context, id int64, until time.Time, provisional bool, expected *time.Time) (bool, error)
}

func openAIQuotaSignal(account *Account, status int, body []byte) (openAIUpstreamError, bool) {
	if account == nil || !account.IsOpenAIOAuth() || status != http.StatusTooManyRequests {
		return openAIUpstreamError{}, false
	}
	e, ok := normalizeOpenAIUpstreamError(body)
	return e, ok && (e.StatusCode == 0 || e.StatusCode == status) &&
		(e.Code == string(OpenAIQuotaExhaustedReason) || e.Type == string(OpenAIQuotaExhaustedReason)) &&
		!isOpenAIImageRateLimitError(status, body) && !isOpenAIRequestScopedCapacityShed("", body)
}

func openAIQuotaDeadline(e openAIUpstreamError, headers http.Header, now time.Time) (time.Time, bool) {
	until, _, _ := openAICodexExhaustedResetAt(ParseCodexRateLimitHeaders(headers), now)
	// Only accept future reset values representable as a provider Unix timestamp.
	const maxUnix = 253402300799 // 9999-12-31
	absolute, _ := strconv.ParseInt(gjson.GetBytes(e.ErrorBody, "error.resets_at").String(), 10, 64)
	if absolute <= now.Unix() || absolute > maxUnix {
		relative, _ := strconv.ParseInt(gjson.GetBytes(e.ErrorBody, "error.resets_in_seconds").String(), 10, 64)
		if relative > 0 && relative <= maxUnix-now.Unix() {
			absolute = now.Unix() + relative
		}
	}
	if absolute > now.Unix() && absolute <= maxUnix && absolute > until.Unix() {
		until = time.Unix(absolute, 0)
	}
	if retry := parseRetryAfterResetTime(headers, now); retry != nil && retry.After(until) {
		until = *retry
	}
	if until.After(now) {
		return until, true
	}
	return time.Unix(now.Unix()+int64(openAIQuotaRefreshInterval/time.Second), 0), false
}

func persistedOpenAIQuotaDeadline(a *Account, now time.Time) time.Time {
	if a == nil || a.RateLimitResetAt == nil || !a.RateLimitResetAt.After(now) {
		return time.Time{}
	}
	if sec, ok := resolveAccountExtraNumber(a.Extra, OpenAIQuotaCooldownExtraKey); ok && int64(sec) > now.Unix() {
		return time.Unix(int64(sec), 0)
	}
	return time.Time{}
}

func (s *RateLimitService) openAIQuotaDeadlineForAccount(a *Account) time.Time {
	if a == nil || !a.IsOpenAIOAuth() {
		return time.Time{}
	}
	now := time.Now()
	until := persistedOpenAIQuotaDeadline(a, now)
	if s != nil {
		if value, ok := s.openAIQuotaUntil.Load(a.ID); ok {
			local := value.(time.Time)
			if !local.After(now) {
				s.openAIQuotaUntil.CompareAndDelete(a.ID, local)
			} else if local.After(until) {
				until = local
			}
		}
	}
	return until
}

func (s *RateLimitService) extendLocalOpenAIQuota(id int64, until time.Time) {
	for {
		old, loaded := s.openAIQuotaUntil.LoadOrStore(id, until)
		if !loaded || !old.(time.Time).Before(until) || s.openAIQuotaUntil.CompareAndSwap(id, old, until) {
			return
		}
	}
}

func (s *RateLimitService) handleOpenAIQuotaExhausted(ctx context.Context, a *Account, e openAIUpstreamError, headers http.Header) {
	ctx, cancel := openAIAccountStateContext(ctx)
	defer cancel()
	until, known := openAIQuotaDeadline(e, headers, time.Now())
	if current := s.openAIQuotaDeadlineForAccount(a); !current.IsZero() && (!known || current.After(until)) {
		until = current
	}
	s.extendLocalOpenAIQuota(a.ID, until)
	s.cancelOpenAI429RecoveryProbe(a.ID)
	if s.accountRepo == nil {
		return
	}
	var err error
	var updated bool
	if repo, ok := s.accountRepo.(openAIQuotaCooldownRepository); ok {
		updated, err = repo.SetOpenAIQuotaCooldown(ctx, a.ID, until, !known, nil)
	} else {
		err = s.accountRepo.SetRateLimited(ctx, a.ID, until)
	}
	if err != nil {
		slog.Error("openai_quota_cooldown_write_failed", "account_id", a.ID, "cooldown_until", until, "error", err)
		return
	}
	persistOpenAI429PlanType(ctx, s.accountRepo, a, e.ErrorBody)
	s.persistOpenAICodexSnapshot(ctx, a, headers)
	notifyOpenAIAutoReset(a.ID)
	if updated && !known && s.openAIQuotaUsage != nil {
		// Only the writer that installed a provisional deadline launches a read,
		// including when concurrent rejections arrive on different replicas.
		go s.refreshOpenAIQuotaDeadline(a.ID, until)
	}
}

func (s *RateLimitService) refreshOpenAIQuotaDeadline(id int64, provisional time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	repo, ok := s.accountRepo.(openAIQuotaCooldownRepository)
	if !ok {
		return
	}
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil || account == nil || account.RateLimitedAt == nil || account.RateLimitResetAt == nil ||
		account.Extra[OpenAIQuotaProvisionalExtraKey] != true || !account.RateLimitResetAt.Equal(provisional) || !persistedOpenAIQuotaDeadline(account, time.Now()).Equal(provisional) {
		return
	}
	usage, err := s.openAIQuotaUsage(ctx, id)
	if err != nil || usage == nil || usage.RateLimit == nil {
		return
	}
	now := time.Now()
	snapshot := codexUsageSnapshotFromQuota(usage, now)
	if until, _, exhausted := openAICodexExhaustedResetAt(snapshot, now); exhausted {
		if until.After(provisional) {
			updated, err := repo.SetOpenAIQuotaCooldown(ctx, id, until, false, account.RateLimitedAt)
			if err != nil {
				slog.Error("openai_quota_refresh_write_failed", "account_id", id, "error", err)
			} else if updated {
				s.extendLocalOpenAIQuota(id, until)
			}
		}
		return
	}
	limits := usage.RateLimit
	if limits.Allowed && !limits.LimitReached && snapshot != nil && snapshot.PrimaryUsedPercent != nil && *snapshot.PrimaryUsedPercent < 100 &&
		(limits.SecondaryWindow == nil || (snapshot.SecondaryUsedPercent != nil && *snapshot.SecondaryUsedPercent < 100)) {
		cleared, err := repo.ClearOpenAIRateLimitIfObserved(ctx, account, false)
		if err == nil && cleared {
			s.openAIQuotaUntil.CompareAndDelete(id, provisional)
		}
	}
}

func (s *OpenAIGatewayService) quotaCooldownFailure(a *Account) *UpstreamFailoverError {
	if s == nil {
		return nil
	}
	if until := s.rateLimitService.openAIQuotaDeadlineForAccount(a); !until.IsZero() {
		return &UpstreamFailoverError{StatusCode: http.StatusTooManyRequests, Reason: OpenAIQuotaExhaustedReason,
			Scope: GatewayFailureScopeAccount, QuotaResetAt: until, QuotaAccountID: a.ID}
	}
	return nil
}

// Long-lived sockets can retain an account snapshot from before another replica
// saw a quota error. Consult the shared scheduler before sending a new turn.
func (s *OpenAIGatewayService) quotaFailureForWSTurn(ctx context.Context, a *Account, payload []byte, originalModel string) error {
	if a == nil || !a.IsOpenAIOAuth() {
		return nil
	}
	failure := s.quotaCooldownFailure(a)
	if failure == nil && s.schedulerSnapshot != nil {
		if latest, err := s.schedulerSnapshot.GetAccount(ctx, a.ID); err == nil && latest != nil {
			failure = s.quotaCooldownFailure(latest)
		}
	}
	if failure == nil {
		return nil
	}
	retry := openAIWSIndependentRetryPayload(payload, originalModel)
	return newOpenAIWSCurrentTurnFailoverError(failure, retry)
}

// captureQuotaClear invalidates only the local deadline observed before an
// explicit administrative reset. Token refresh and ordinary successful turns
// never use this path.
func captureOpenAIQuotaClear(blocker AccountRuntimeBlocker, id int64) func() {
	gw, ok := blocker.(*OpenAIGatewayService)
	if !ok || gw == nil || gw.rateLimitService == nil {
		return func() {}
	}
	state := &gw.rateLimitService.openAIQuotaUntil
	observed, present := state.Load(id)
	return func() {
		if present {
			state.CompareAndDelete(id, observed)
		}
	}
}

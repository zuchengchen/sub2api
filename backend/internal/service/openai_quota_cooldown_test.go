//go:build unit

package service

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type quotaCooldownTestRepo struct {
	AccountRepository
	mu      sync.Mutex
	account Account
	settled chan struct{}
}

func (r *quotaCooldownTestRepo) GetByID(context.Context, int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.account
	a.Extra = cloneExtraJSON(r.account.Extra)
	return &a, nil
}

func (r *quotaCooldownTestRepo) SetOpenAIQuotaCooldown(_ context.Context, _ int64, until time.Time, provisional bool, expected *time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if expected != nil && (r.account.RateLimitedAt == nil || !r.account.RateLimitedAt.Equal(*expected)) {
		return false, nil
	}
	if r.account.RateLimitResetAt != nil && !until.After(*r.account.RateLimitResetAt) {
		return false, nil
	}
	now := time.Now()
	r.account.RateLimitedAt, r.account.RateLimitResetAt = &now, &until
	r.account.Extra = map[string]any{OpenAIQuotaCooldownExtraKey: float64(until.Unix()), OpenAIQuotaProvisionalExtraKey: provisional}
	if expected != nil {
		close(r.settled)
	}
	return true, nil
}

func (r *quotaCooldownTestRepo) ClearOpenAIRateLimitIfObserved(_ context.Context, a *Account, _ bool) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer close(r.settled)
	if r.account.RateLimitedAt == nil || !r.account.RateLimitedAt.Equal(*a.RateLimitedAt) {
		return false, nil
	}
	r.account.RateLimitedAt, r.account.RateLimitResetAt = nil, nil
	delete(r.account.Extra, OpenAIQuotaCooldownExtraKey)
	return true, nil
}

func TestOpenAIQuotaSignalRequiresAccountEvidence(t *testing.T) {
	a := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	for _, tc := range []struct {
		body string
		hard bool
	}{
		{`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`, true},
		{`{"type":"response.failed","response":{"status":"failed","error":{"code":"usage_limit_reached"}}}`, true},
		{`{"type":"error","code":"usage_limit_reached","message":"quota exhausted"}`, true},
		{`{"error":{"code":"rate_limit_exceeded","message":"The usage limit has been reached"}}`, false},
		{`{"error":{"code":"turn_rate_limit_exceeded","message":"usage_limit_reached"}}`, false},
		{`{"error":{"type":"usage_limit_reached","message":"rate limit for limit gpt-image"}}`, false},
		{`{"error":{"code":"usage_limit_reached","code":"rate_limit_exceeded"}}`, false},
	} {
		_, ok := openAIQuotaSignal(a, 429, []byte(tc.body))
		require.Equal(t, tc.hard, ok, tc.body)
	}
	body := []byte(`{"error":{"type":"usage_limit_reached","resets_at":-1,"resets_in_seconds":"900"}}`)
	e, ok := openAIQuotaSignal(a, 429, body)
	require.True(t, ok)
	now := time.Now().Truncate(time.Second)
	until, known := openAIQuotaDeadline(e, nil, now)
	require.True(t, known)
	require.True(t, now.Add(15*time.Minute).Equal(until))
	for _, other := range []*Account{{Platform: PlatformGrok, Type: AccountTypeOAuth}, {Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, {Platform: PlatformOpenAI, Type: AccountTypeSetupToken}} {
		_, ok := openAIQuotaSignal(other, 429, body)
		require.False(t, ok)
	}
}

func TestOpenAIQuotaCooldownMissingResetCoalescesAndRechecks(t *testing.T) {
	for _, state := range []string{"exhausted", "available", "newer_rejection"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			a := Account{ID: 13501, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
			repo := &quotaCooldownTestRepo{account: a, settled: make(chan struct{})}
			s := NewRateLimitService(repo, nil, nil, nil)
			gateway := &OpenAIGatewayService{rateLimitService: s}
			started, resume := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			s.openAIQuotaUsage = func(context.Context, int64) (*OpenAIQuotaUsage, error) {
				if calls.Add(1) == 1 {
					close(started)
				}
				<-resume
				w := &OpenAIRateLimitWindow{UsedPercent: 15, ResetAfterSeconds: 3600, LimitWindowSeconds: 18000}
				if state == "exhausted" {
					w.UsedPercent = 100
				}
				return &OpenAIQuotaUsage{RateLimit: &OpenAIRateLimit{Allowed: state != "exhausted", LimitReached: state == "exhausted", PrimaryWindow: w}}, nil
			}
			body := []byte(`{"error":{"type":"usage_limit_reached"}}`)
			require.True(t, gateway.handleOpenAIAccountUpstreamError(ctx, &a, 429, nil, body))
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("quota query did not start")
			}
			for range 5 {
				require.True(t, gateway.handleOpenAIAccountUpstreamError(ctx, &a, 429, nil, body))
			}
			stale := a
			gateway.ReportOpenAIAccountScheduleResult(&stale, "gpt-5.2", true, nil)
			require.True(t, gateway.isOpenAIAccountRequestRuntimeBlocked(&stale, "gpt-5.2", false))
			raw := []byte(`{"type":"response.create","model":"gpt-5.2","input":"second turn"}`)
			payload, wrapped := OpenAIWSCurrentTurnRetryPayload(gateway.quotaFailureForWSTurn(ctx, &stale, raw))
			require.True(t, wrapped)
			require.Equal(t, raw, payload)
			fail := gateway.newOpenAIAccountFailoverError(&a, 429, nil, body, "", false, true)
			require.False(t, fail.RetryableOnSameAccount)
			require.True(t, fail.ShouldRetryNextAccount())
			require.WithinDuration(t, time.Now().Add(time.Minute), fail.QuotaResetAt, 2*time.Second)
			if state == "newer_rejection" {
				known := []byte(fmt.Sprintf(`{"type":"error","error":{"code":"usage_limit_reached","resets_at":%d}}`, time.Now().Add(2*time.Hour).Unix()))
				require.True(t, s.HandleUpstreamError(ctx, &a, http.StatusTooManyRequests, nil, known))
			}
			close(resume)
			select {
			case <-repo.settled:
			case <-time.After(2 * time.Second):
				t.Fatal("quota query did not settle")
			}
			require.Equal(t, int32(1), calls.Load())
			switch state {
			case "available":
				require.Eventually(t, func() bool { return gateway.quotaCooldownFailure(&stale) == nil }, time.Second, time.Millisecond)
			case "newer_rejection":
				require.Greater(t, time.Until(gateway.quotaCooldownFailure(&stale).QuotaResetAt), 119*time.Minute)
			case "exhausted":
				require.Eventually(t, func() bool { return time.Until(gateway.quotaCooldownFailure(&stale).QuotaResetAt) > 59*time.Minute }, time.Second, time.Millisecond)
			}
		})
	}
}

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type consecutive429Repo struct {
	mockAccountRepoForTest
	rateLimitCalls int
	lastRateLimit  int64
}

func (r *consecutive429Repo) SetRateLimited(_ context.Context, id int64, _ time.Time) error {
	r.rateLimitCalls++
	r.lastRateLimit = id
	return nil
}

func (r *consecutive429Repo) UpdateExtra(_ context.Context, _ int64, _ map[string]any) error {
	return nil
}

func (r *consecutive429Repo) BulkUpdate(_ context.Context, ids []int64, _ AccountBulkUpdate) (int64, error) {
	return int64(len(ids)), nil
}

func resetOpenAI429TestState(t *testing.T) {
	t.Helper()
	quota429Mu.Lock()
	quota429Counts = map[int64]int{}
	quota429Mu.Unlock()
	openAINearLimitMu.Lock()
	openAINearLimitState = map[int64]*openAINearLimitRuntimeState{}
	openAINearLimitMu.Unlock()
}

func openai429Headers() http.Header {
	h := http.Header{}
	h.Set("x-codex-secondary-used-percent", "100")
	h.Set("x-codex-secondary-reset-after-seconds", "18000")
	h.Set("x-codex-secondary-window-minutes", "300")
	return h
}

func TestHandleUpstreamError_OpenAI429SoftThenFormalCooldown(t *testing.T) {
	resetOpenAI429TestState(t)
	repo := &consecutive429Repo{}
	svc := NewRateLimitService(repo, nil, nil, nil)
	account := &Account{ID: 9001, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}

	for i := 1; i <= 29; i++ {
		shouldDisable := svc.HandleUpstreamError(context.Background(), account, http.StatusTooManyRequests, openai429Headers(), nil)
		require.False(t, shouldDisable, "count %d", i)
		require.Equal(t, 0, repo.rateLimitCalls, "soft 429 %d must not SetRateLimited", i)
		require.Equal(t, i, openAI429Count(account.ID))
	}

	shouldDisable := svc.HandleUpstreamError(context.Background(), account, http.StatusTooManyRequests, openai429Headers(), nil)
	require.False(t, shouldDisable)
	require.Equal(t, 1, repo.rateLimitCalls)
	require.Equal(t, account.ID, repo.lastRateLimit)
	require.Equal(t, quota429CooldownThreshold, openAI429Count(account.ID))

	repo.rateLimitCalls = 0
	_ = svc.HandleUpstreamError(context.Background(), account, http.StatusTooManyRequests, openai429Headers(), nil)
	require.Equal(t, 1, repo.rateLimitCalls)
	require.Equal(t, quota429CooldownThreshold, openAI429Count(account.ID))
}

func TestHandleUpstreamError_OpenAI429SuccessClearsCount(t *testing.T) {
	resetOpenAI429TestState(t)
	repo := &consecutive429Repo{}
	svc := NewRateLimitService(repo, nil, nil, nil)
	account := &Account{ID: 9002, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	for i := 0; i < 12; i++ {
		svc.HandleUpstreamError(context.Background(), account, http.StatusTooManyRequests, openai429Headers(), nil)
	}
	require.Equal(t, 12, openAI429Count(account.ID))
	ResetOpenAI429Counter(account.ID)
	require.Equal(t, 0, openAI429Count(account.ID))

	svc.HandleUpstreamError(context.Background(), account, http.StatusTooManyRequests, openai429Headers(), nil)
	require.Equal(t, 1, openAI429Count(account.ID))
	require.Equal(t, 0, repo.rateLimitCalls)
}

func TestHandleUpstreamError_OpenAI429FiveXXDoesNotChangeCount(t *testing.T) {
	resetOpenAI429TestState(t)
	repo := &consecutive429Repo{}
	svc := NewRateLimitService(repo, nil, nil, nil)
	account := &Account{ID: 9003, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	for i := 0; i < 5; i++ {
		svc.HandleUpstreamError(context.Background(), account, http.StatusTooManyRequests, openai429Headers(), nil)
	}
	svc.HandleUpstreamError(context.Background(), account, http.StatusBadGateway, http.Header{}, []byte(`{"error":"bad gateway"}`))
	svc.HandleUpstreamError(context.Background(), account, http.StatusRequestTimeout, http.Header{}, []byte(`{"error":"timeout"}`))
	require.Equal(t, 5, openAI429Count(account.ID))
	svc.HandleUpstreamError(context.Background(), account, http.StatusTooManyRequests, openai429Headers(), nil)
	require.Equal(t, 6, openAI429Count(account.ID))
	require.Equal(t, 0, repo.rateLimitCalls)
}

func TestHandleUpstreamError_OpenAI429DisableCoolingClearsAndSkips(t *testing.T) {
	resetOpenAI429TestState(t)
	repo := &consecutive429Repo{}
	svc := NewRateLimitService(repo, nil, nil, nil)
	account := &Account{
		ID: 9004, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"disable_429_cooling": "true"},
	}
	for i := 0; i < 5; i++ {
		shouldDisable := svc.HandleUpstreamError(context.Background(), account, http.StatusTooManyRequests, openai429Headers(), nil)
		require.False(t, shouldDisable)
	}
	require.Equal(t, 0, openAI429Count(account.ID))
	require.Equal(t, 0, repo.rateLimitCalls)
}

func TestHandleUpstreamError_OpenAI429ShadowSkipped(t *testing.T) {
	resetOpenAI429TestState(t)
	repo := &consecutive429Repo{}
	svc := NewRateLimitService(repo, nil, nil, nil)
	parent := int64(1)
	shadow := &Account{ID: 9005, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &parent}
	svc.HandleUpstreamError(context.Background(), shadow, http.StatusTooManyRequests, openai429Headers(), nil)
	require.Equal(t, 0, openAI429Count(shadow.ID))
	require.Equal(t, 0, repo.rateLimitCalls)
}

func TestNextOpenAI429CooldownCapsAtThreshold(t *testing.T) {
	resetOpenAI429TestState(t)
	for i := 1; i <= 35; i++ {
		count, should := nextOpenAI429Cooldown(42)
		if i < quota429CooldownThreshold {
			require.Equal(t, i, count)
			require.False(t, should)
		} else {
			require.Equal(t, quota429CooldownThreshold, count)
			require.True(t, should)
		}
	}
}

func TestOpenAI429ReportSuccessResetsCounter(t *testing.T) {
	resetOpenAI429TestState(t)
	nextOpenAI429Cooldown(77)
	nextOpenAI429Cooldown(77)
	require.Equal(t, 2, openAI429Count(77))
	svc := &OpenAIGatewayService{}
	svc.ReportOpenAIAccountScheduleResult(&Account{ID: 77, Platform: PlatformOpenAI}, "gpt-5.1", true, nil)
	require.Equal(t, 0, openAI429Count(77))
}

func TestOpenAI429OAuthRetryDoesNotZeroSoftCount(t *testing.T) {
	resetOpenAI429TestState(t)
	repo := &consecutive429Repo{}
	svc := NewRateLimitService(repo, nil, nil, nil)
	gw := &OpenAIGatewayService{rateLimitService: svc}
	svc.SetAccountRuntimeBlocker(gw)
	account := &Account{ID: 9006, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	for i := 0; i < 3; i++ {
		svc.HandleUpstreamError(context.Background(), account, http.StatusTooManyRequests, openai429Headers(), nil)
	}
	require.Equal(t, 3, openAI429Count(account.ID))
	require.Equal(t, 0, repo.rateLimitCalls)

	transientHeaders := http.Header{}
	transientBody := []byte(`{"error":{"type":"rate_limit_error","message":"try again"}}`)
	require.True(t, gw.ShouldRetryOpenAIOAuth429(account, transientHeaders, transientBody),
		"same-account OAuth retry must still be eligible after soft 429 counts")
	svc.handle429(context.Background(), account, transientHeaders, transientBody)
	require.Equal(t, 3, openAI429Count(account.ID), "OAuth same-account retry must not zero the soft 429 counter")
	require.Equal(t, 0, repo.rateLimitCalls, "OAuth retry must not SetRateLimited on soft counts")
}

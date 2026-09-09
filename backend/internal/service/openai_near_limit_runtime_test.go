package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func nearLimitExtra(used float64, age time.Duration) map[string]any {
	return map[string]any{
		"codex_5h_used_percent":   used,
		"codex_5h_reset_at":       time.Now().Add(time.Hour).Format(time.RFC3339),
		"codex_usage_updated_at":  time.Now().Add(-age).Format(time.RFC3339),
		"auto_pause_5h_disabled":  true,
		"auto_pause_7d_disabled":  true,
	}
}

func TestClassifyOpenAINearLimitCandidate_ThresholdAndStale(t *testing.T) {
	now := time.Now()
	fresh95 := &Account{ID: 1, Platform: PlatformOpenAI, Extra: nearLimitExtra(95, time.Minute)}
	cand, ok := classifyOpenAINearLimitCandidate(fresh95, now)
	require.True(t, ok)
	require.Equal(t, "5h", cand.window)
	require.Equal(t, 95.0, cand.usedPercent)

	below := &Account{ID: 2, Platform: PlatformOpenAI, Extra: nearLimitExtra(94.9, time.Minute)}
	_, ok = classifyOpenAINearLimitCandidate(below, now)
	require.False(t, ok)

	stale := &Account{ID: 3, Platform: PlatformOpenAI, Extra: nearLimitExtra(99, 3*time.Hour)}
	_, ok = classifyOpenAINearLimitCandidate(stale, now)
	require.False(t, ok)

	missing := &Account{ID: 4, Platform: PlatformOpenAI, Extra: map[string]any{"codex_5h_used_percent": 99.0}}
	_, ok = classifyOpenAINearLimitCandidate(missing, now)
	require.False(t, ok)

	grok := &Account{ID: 5, Platform: PlatformGrok, Extra: nearLimitExtra(99, time.Minute)}
	_, ok = classifyOpenAINearLimitCandidate(grok, now)
	require.False(t, ok)
}

func TestOpenAINearLimitHigherPriorityOrdering(t *testing.T) {
	a := openAINearLimitCandidate{account: &Account{ID: 2, Priority: 5}, usedPercent: 98, resetAt: time.Now().Add(2 * time.Hour)}
	b := openAINearLimitCandidate{account: &Account{ID: 1, Priority: 5}, usedPercent: 95, resetAt: time.Now().Add(time.Hour)}
	require.True(t, openAINearLimitHigherPriority(a, b))
	samePctEarlier := openAINearLimitCandidate{account: &Account{ID: 3, Priority: 5}, usedPercent: 98, resetAt: time.Now().Add(time.Hour)}
	require.True(t, openAINearLimitHigherPriority(samePctEarlier, a))
}

func TestNearLimitEffectiveOpenAIAccountConcurrencyCap(t *testing.T) {
	cfg := &config.Config{Gateway: config.GatewayConfig{OpenAINearLimitMaxConcurrency: 50}}
	acc := &Account{Concurrency: 100}
	require.Equal(t, 50, effectiveOpenAIAccountConcurrency(acc, cfg))
	acc.Concurrency = 10
	require.Equal(t, 10, effectiveOpenAIAccountConcurrency(acc, cfg))
	acc.Concurrency = 0
	require.Equal(t, 50, effectiveOpenAIAccountConcurrency(acc, cfg))
	require.Equal(t, defaultOpenAINearLimitMaxConcurrency, effectiveOpenAIAccountConcurrency(acc, &config.Config{}))
}

func TestMarkOpenAINearLimit429BackoffAndLease(t *testing.T) {
	resetOpenAI429TestState(t)
	acc := &Account{ID: 44, Platform: PlatformOpenAI, Extra: nearLimitExtra(96, time.Minute)}
	MarkOpenAINearLimit429(acc)
	require.False(t, canClaimOpenAINearLimitPreference(acc.ID, time.Now(), true))
	require.True(t, canClaimOpenAINearLimitPreference(acc.ID, time.Now(), false))
	require.True(t, canClaimOpenAINearLimitPreference(acc.ID, time.Now().Add(3*time.Second), true))

	require.True(t, claimOpenAINearLimitPreference(acc.ID, 90*time.Second))
	require.False(t, canClaimOpenAINearLimitPreference(acc.ID, time.Now(), false))
	releaseOpenAINearLimitPreference(acc.ID)
	require.True(t, canClaimOpenAINearLimitPreference(acc.ID, time.Now().Add(3*time.Second), true))
}

func TestUpdateOpenAINearLimitTTFTState(t *testing.T) {
	resetOpenAI429TestState(t)
	acc := &Account{ID: 45, Platform: PlatformOpenAI, Extra: nearLimitExtra(97, time.Minute)}
	cfg := &config.Config{Gateway: config.GatewayConfig{
		OpenAINearLimitTTFTThresholdMS:    15000,
		OpenAINearLimitTTFTBackoffSeconds: 30,
	}}
	updateOpenAINearLimitTTFTState(acc, 16*time.Second, cfg)
	require.False(t, canClaimOpenAINearLimitPreference(acc.ID, time.Now(), true))
	updateOpenAINearLimitTTFTState(acc, 2*time.Second, cfg)
	require.True(t, canClaimOpenAINearLimitPreference(acc.ID, time.Now(), true))
}

func TestWithSkipOpenAINearLimitAccounts(t *testing.T) {
	ctx := WithSkipOpenAINearLimitAccounts(context.Background())
	require.True(t, skipOpenAINearLimitAccounts(ctx))
	require.False(t, skipOpenAINearLimitAccounts(context.Background()))
}

func TestSelectAccountForModelWithExclusions_PrefersNearLimitOverPriority(t *testing.T) {
	resetOpenAI429TestState(t)
	ctx := context.Background()
	near := Account{
		ID: 41001, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 9,
		Extra: nearLimitExtra(98, time.Minute),
	}
	normal := Account{
		ID: 41002, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0,
		Extra: map[string]any{
			"codex_5h_used_percent":  10.0,
			"codex_usage_updated_at": time.Now().Format(time.RFC3339),
		},
	}
	svc := &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{near, normal}},
		cfg:         &config.Config{Gateway: config.GatewayConfig{OpenAINearLimitMaxConcurrency: 20}},
	}
	account, err := svc.SelectAccountForModelWithExclusions(ctx, nil, "", "gpt-5.1", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, int64(41001), account.ID)
}

func TestSelectAccountForModelWithExclusions_StaleSnapshotNotNearLimit(t *testing.T) {
	resetOpenAI429TestState(t)
	ctx := context.Background()
	stale := Account{
		ID: 41101, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 9,
		Extra: nearLimitExtra(99, 3*time.Hour),
	}
	fresh := Account{
		ID: 41102, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0,
		Extra: map[string]any{
			"codex_5h_used_percent":  10.0,
			"codex_usage_updated_at": time.Now().Format(time.RFC3339),
		},
	}
	svc := &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{stale, fresh}},
		cfg:         &config.Config{},
	}
	account, err := svc.SelectAccountForModelWithExclusions(ctx, nil, "", "gpt-5.1", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, int64(41102), account.ID, "stale 99% must not enter near-limit preference")
}

func TestGatewayTrySelectOpenAINearLimitTargetUsesDBConcurrency(t *testing.T) {
	resetOpenAI429TestState(t)
	acc := &Account{
		ID: 41201, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 100, Priority: 0,
		Extra: nearLimitExtra(96, time.Minute),
	}
	MarkOpenAINearLimit429(acc)
	require.False(t, canClaimOpenAINearLimitPreference(acc.ID, time.Now(), true))

	svc := &GatewayService{}
	require.NotNil(t, svc)
	// Path difference: gateway path does not consult probe backoff.
	require.False(t, canClaimOpenAINearLimitPreference(acc.ID, time.Now(), true))
}

func TestStreamFirstTokenTimeoutDefaultAndOff(t *testing.T) {
	require.Equal(t, 60*time.Second, streamFirstTokenTimeout(nil))
	require.Equal(t, time.Duration(0), streamFirstTokenTimeout(&config.Config{}))
	require.Equal(t, 60*time.Second, streamFirstTokenTimeout(&config.Config{Gateway: config.GatewayConfig{StreamFirstTokenTimeout: 60}}))
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{StreamFirstTokenTimeout: 60}}}
	require.Equal(t, 60*time.Second, svc.httpStreamFirstTokenTimeout())
}

func TestOpenAIHTTPStreamFirstTokenTimeoutIgnoresPreamble(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		StreamFirstTokenTimeout: 1,
		MaxLineSize:             defaultMaxLineSize,
	}}}
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	go func() {
		_, _ = pw.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_http_tt\"}}\n\n"))
		_, _ = pw.Write([]byte("data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_http_tt\"}}\n\n"))
		time.Sleep(3 * time.Second)
	}()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: pr}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, time.Now(), "model", "model")
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode, "err=%v body=%s", err, string(failoverErr.ResponseBody))
	require.Contains(t, string(failoverErr.ResponseBody), "first_output_timeout")
}

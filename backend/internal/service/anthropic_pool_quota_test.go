//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const kiroAllCredentialsQuotaBody = `{"error":{"type":"rate_limit_error","message":"所有可用凭据均已用尽订阅额度或超额额度，下个计费周期自动恢复"}}`

func newAnthropicQuotaPoolAccount() *Account {
	account := newAnthropicAPIKeyAccountForTest()
	account.Credentials["pool_mode"] = true
	account.Credentials["pool_mode_retry_count"] = 10
	return account
}

func TestAnthropicPoolQuotaClassification(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"observed pool exhaustion", 429, kiroAllCredentialsQuotaBody, true},
		{"explicit whole pool code", 429, `{"error":{"code":"all_credentials_quota_exhausted","message":"out of quota"}}`, true},
		{"english whole pool exhaustion", 429, `{"error":{"message":"All available credentials have exhausted their subscription or overage quota. Retry next billing cycle."}}`, true},
		{"one member quota", 429, `{"error":{"code":"insufficient_quota","message":"quota exhausted"}}`, false},
		{"ordinary rate limit", 429, `{"error":{"type":"rate_limit_error","message":"Upstream rate limit exceeded. Retry later."}}`, false},
		{"echoed request", 429, `{"error":{"message":"rate limited"},"input":"所有可用凭据均已用尽订阅额度或超额额度"}`, false},
		{"unstructured error", 429, `所有可用凭据均已用尽订阅额度或超额额度`, false},
		{"wrong status", 502, kiroAllCredentialsQuotaBody, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isAnthropicPoolQuotaExhausted(newAnthropicQuotaPoolAccount(), tt.status, []byte(tt.body)))
		})
	}
	for _, change := range []func(*Account){
		func(a *Account) { a.Platform = PlatformOpenAI },
		func(a *Account) { a.Type = AccountTypeOAuth },
		func(a *Account) { a.Credentials["pool_mode"] = false },
	} {
		account := newAnthropicQuotaPoolAccount()
		change(account)
		require.False(t, isAnthropicPoolQuotaExhausted(account, 429, []byte(kiroAllCredentialsQuotaBody)))
	}
}

func TestAnthropicPoolQuotaCooldown(t *testing.T) {
	for _, tt := range []struct {
		name      string
		headers   func(time.Time) http.Header
		existing  time.Duration
		wantDelay time.Duration
	}{
		{"bounded probe without reset", func(time.Time) http.Header { return nil }, 0, 5 * time.Minute},
		{"retry after seconds", func(time.Time) http.Header { return http.Header{"Retry-After": {"30"}} }, 0, 30 * time.Second},
		{"retry after HTTP date", func(now time.Time) http.Header {
			return http.Header{"Retry-After": {now.Add(time.Hour).UTC().Format(http.TimeFormat)}}
		}, 0, time.Hour},
		{"known reset", func(now time.Time) http.Header {
			return http.Header{"Anthropic-Ratelimit-Unified-Reset": {strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10)}}
		}, 0, 2 * time.Hour},
		{"latest authoritative reset", func(now time.Time) http.Header {
			return http.Header{"Retry-After": {"30"}, "Anthropic-Ratelimit-Unified-Reset": {strconv.FormatInt(now.Add(time.Hour).Unix(), 10)}}
		}, 0, time.Hour},
		{"expired reset uses probe", func(time.Time) http.Header {
			return http.Header{"Retry-After": {"-10"}, "Anthropic-Ratelimit-Unified-Reset": {"1"}}
		}, 0, 5 * time.Minute},
		{"does not shorten an existing cooldown", func(time.Time) http.Header { return nil }, 3 * time.Hour, 3 * time.Hour},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := &rateLimitAccountRepoStub{}
			blocker := &runtimeBlockRecorder{}
			svc := NewRateLimitService(repo, nil, &config.Config{}, nil)
			svc.SetAccountRuntimeBlocker(blocker)
			account := newAnthropicQuotaPoolAccount()
			now := time.Now()
			if tt.existing > 0 {
				until := now.Add(tt.existing)
				account.RateLimitResetAt = &until
			}
			shouldDisable := svc.HandleUpstreamError(context.Background(), account, 429, tt.headers(now), []byte(kiroAllCredentialsQuotaBody))
			require.True(t, shouldDisable)
			require.Equal(t, 1, repo.rateLimitedCalls)
			require.WithinDuration(t, now.Add(tt.wantDelay), repo.lastRateLimitedAt, time.Second)
			require.Zero(t, repo.setErrorCalls, "the pool must recover without an admin re-enable")
			require.Zero(t, repo.tempCalls)
			require.Equal(t, StatusActive, account.Status)
			require.Equal(t, []string{"anthropic_pool_quota_exhausted"}, blocker.reasons)
		})
	}
}

func TestAnthropicPoolQuotaKeepsExplicitErrorPolicyAndOrdinary429(t *testing.T) {
	for _, customExclusion := range []bool{false, true} {
		repo := &rateLimitAccountRepoStub{}
		svc := NewRateLimitService(repo, nil, &config.Config{}, nil)
		account := newAnthropicQuotaPoolAccount()
		body := []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)
		if customExclusion {
			account.Credentials["custom_error_codes_enabled"] = true
			account.Credentials["custom_error_codes"] = []any{float64(401)}
			body = []byte(kiroAllCredentialsQuotaBody)
		}
		require.False(t, svc.HandleUpstreamError(context.Background(), account, 429, nil, body))
		require.Zero(t, repo.rateLimitedCalls)
		require.Zero(t, repo.setErrorCalls)
	}
}

func TestAnthropicPoolFailoverHonorsRetryAfter(t *testing.T) {
	for _, tt := range []struct {
		name       string
		retryAfter string
		body       string
		wantRetry  bool
		wantDelay  time.Duration
	}{
		{"ordinary retry", "", `{"error":{"message":"rate limited"}}`, true, 0},
		{"upstream delay", "2", `{"error":{"message":"rate limited"}}`, true, 2 * time.Second},
		{"invalid delay", "invalid", `{"error":{"message":"rate limited"}}`, true, 0},
		{"long delay selects another account", "3600", `{"error":{"message":"rate limited"}}`, false, 0},
		{"whole pool exhausted", "2", kiroAllCredentialsQuotaBody, false, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {tt.retryAfter}}}
			err := newGatewayHTTPFailoverError(newAnthropicQuotaPoolAccount(), resp, []byte(tt.body), false)
			require.Equal(t, tt.wantRetry, err.RetryableOnSameAccount)
			require.Equal(t, tt.wantDelay, err.SameAccountRetryDelay)
			require.Equal(t, tt.retryAfter, err.ResponseHeaders.Get("Retry-After"))
		})
	}
}

func TestAnthropicPoolQuotaAnd502ForwardPaths(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"native", "passthrough", "responses", "chat_completions"} {
		for _, status := range []int{429, 502} {
			t.Run(path+"/"+strconv.Itoa(status), func(t *testing.T) {
				errorBody := kiroAllCredentialsQuotaBody
				if status == 502 {
					errorBody = `{"error":{"type":"upstream_error","message":"Upstream API request failed."}}`
				}
				upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{{
					StatusCode: status, Header: http.Header{"X-Request-Id": {"pool-regression"}},
					Body: io.NopCloser(strings.NewReader(errorBody)),
				}}}
				repo := &rateLimitAccountRepoStub{}
				cfg := &config.Config{}
				svc := &GatewayService{
					cfg: cfg, httpUpstream: upstream, tlsFPProfileService: &TLSFingerprintProfileService{},
					rateLimitService: NewRateLimitService(repo, nil, cfg, nil),
				}
				account := newAnthropicQuotaPoolAccount()
				account.Extra["anthropic_passthrough"] = path == "passthrough"
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"synthetic quota regression"}],"max_tokens":100}`)
				var err error
				var result *ForwardResult
				switch path {
				case "responses":
					result, err = svc.ForwardAsResponses(context.Background(), c, account, []byte(`{"model":"claude-sonnet-4-5","input":"synthetic quota regression"}`), nil)
				case "chat_completions":
					result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
				default:
					result, err = svc.Forward(context.Background(), c, account, &ParsedRequest{Model: "claude-sonnet-4-5", Body: NewRequestBodyRef(body)})
				}
				require.Nil(t, result)
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				require.Equal(t, status, failover.StatusCode)
				require.False(t, failover.RetryableOnSameAccount)
				require.True(t, failover.ShouldRetryNextAccount())
				require.Equal(t, "pool-regression", failover.ResponseHeaders.Get("X-Request-Id"))
				require.Equal(t, 1, upstream.callCount)
				require.False(t, c.Writer.Written(), "failover is allowed only before downstream output")
				require.Empty(t, rec.Body.String())
				require.Zero(t, repo.setErrorCalls)
				if status == 429 {
					require.Equal(t, 1, repo.rateLimitedCalls)
				} else {
					require.Zero(t, repo.rateLimitedCalls, "a temporary 502 must not become a quota cooldown")
				}
			})
		}
	}
}

func TestAnthropicPoolQuotaSkipsInternalRetryForExcluded429(t *testing.T) {
	svc := &GatewayService{}
	account := newAnthropicQuotaPoolAccount()
	account.Credentials["custom_error_codes_enabled"] = true
	account.Credentials["custom_error_codes"] = []any{float64(401)}
	resp := &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(kiroAllCredentialsQuotaBody))}
	require.False(t, svc.shouldRetryUpstreamResponse(account, resp))
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.JSONEq(t, kiroAllCredentialsQuotaBody, string(body), "side effects and client error must retain the body")
}

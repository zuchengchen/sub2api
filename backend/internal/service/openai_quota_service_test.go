package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// ── stub helpers ─────────────────────────────────────────────────────────────

// stubQuotaAccountRepo 是多账号 AccountRepository stub，仅实现 GetByID。
type stubQuotaAccountRepo struct {
	AccountRepository
	accounts         map[int64]*Account
	extraUpdates     map[int64]map[string]any
	extraUpdateCalls int
	extraUpdateErr   error
}

func (r *stubQuotaAccountRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	acc, ok := r.accounts[id]
	if !ok {
		return nil, fmt.Errorf("account %d not found", id)
	}
	return acc, nil
}

func (r *stubQuotaAccountRepo) UpdateCredentials(_ context.Context, id int64, credentials map[string]any) error {
	acc, ok := r.accounts[id]
	if !ok {
		return fmt.Errorf("account %d not found", id)
	}
	acc.Credentials = credentials
	return nil
}

func (r *stubQuotaAccountRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	if r.extraUpdateErr != nil {
		return r.extraUpdateErr
	}
	r.extraUpdateCalls++
	if r.extraUpdates == nil {
		r.extraUpdates = make(map[int64]map[string]any)
	}
	r.extraUpdates[id] = updates
	return nil
}

// stubQuotaTokenCache 实现 OpenAITokenCache，返回预设静态 token。
type stubQuotaTokenCache struct {
	tokens map[string]string
}

func (c *stubQuotaTokenCache) GetAccessToken(_ context.Context, key string) (string, error) {
	if t, ok := c.tokens[key]; ok {
		return t, nil
	}
	return "", errors.New("token not found")
}

func (c *stubQuotaTokenCache) SetAccessToken(_ context.Context, _ string, _ string, _ time.Duration) error {
	return nil
}

func (c *stubQuotaTokenCache) DeleteAccessToken(_ context.Context, _ string) error { return nil }

func (c *stubQuotaTokenCache) AcquireRefreshLock(_ context.Context, _ string, _ time.Duration) (bool, error) {
	return true, nil
}

func (c *stubQuotaTokenCache) ReleaseRefreshLock(_ context.Context, _ string) error { return nil }

// newQuotaRedirectingFactory 返回 PrivacyClientFactory，将请求重定向到 httptest.Server。
func newQuotaRedirectingFactory(srv *httptest.Server) PrivacyClientFactory {
	targetURL, _ := url.Parse(srv.URL)
	return func(_ string) (*req.Client, error) {
		c := req.C().WrapRoundTripFunc(func(rt req.RoundTripper) req.RoundTripFunc {
			return func(r *req.Request) (*req.Response, error) {
				r.URL.Scheme = targetURL.Scheme
				r.URL.Host = targetURL.Host
				return rt.RoundTrip(r)
			}
		})
		return c, nil
	}
}

func TestResetCreditUsesAccountCredentials(t *testing.T) {
	account := &Account{
		ID: 204, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "account-reset"},
	}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	tokenCache := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(account): "fake-token"}}
	tokenProvider := NewOpenAITokenProvider(repo, tokenCache, nil)
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/backend-api/wham/rate-limit-reset-credits/consume", r.URL.Path)
		require.Equal(t, "account-reset", r.Header.Get("chatgpt-account-id"))
		require.Equal(t, "Bearer fake-token", r.Header.Get("authorization"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"code":"ok","windows_reset":2}`))
	}))
	defer srv.Close()

	svc := NewOpenAIQuotaService(repo, nil, tokenProvider, newQuotaRedirectingFactory(srv), nil)
	result, err := svc.ResetCredit(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, "ok", result.Code)
	require.Equal(t, 2, result.WindowsReset)
	require.Len(t, body, 1)
	require.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, body["redeem_request_id"])
}

func TestResetCreditTargetedSendsStableCreditAndRedeemIDs(t *testing.T) {
	account := &Account{
		ID: 203, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "account-targeted"},
	}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	tokenCache := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(account): "fake-token"}}
	tokenProvider := NewOpenAITokenProvider(repo, tokenCache, nil)
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/backend-api/wham/rate-limit-reset-credits/consume", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"code":"ok","windows_reset":1}`))
	}))
	defer srv.Close()

	svc := NewOpenAIQuotaService(repo, nil, tokenProvider, newQuotaRedirectingFactory(srv), nil)
	result, err := svc.ResetCreditTargeted(context.Background(), account.ID, "credit-123", "redeem-456")
	require.NoError(t, err)
	require.Equal(t, "ok", result.Code)
	require.Equal(t, map[string]string{
		"credit_id": "credit-123", "redeem_request_id": "redeem-456",
	}, body)
}

func TestResetCreditAgentIdentityUsesAssertionAndRecoversInvalidTaskOnce(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	require.NoError(t, err)
	account := &Account{
		ID:       201,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"auth_mode":          OpenAIAuthModeAgentIdentity,
			"agent_runtime_id":   "runtime-reset-recovery",
			"agent_private_key":  base64.StdEncoding.EncodeToString(der),
			"task_id":            "task-reset-old",
			"chatgpt_account_id": "account-reset-recovery",
		},
	}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	resetCalls := 0
	registerCalls := 0
	var assertions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if strings.Contains(r.URL.Path, "/task/register") {
			registerCalls++
			_, _ = w.Write([]byte(`{"task_id":"task-reset-new"}`))
			return
		}
		resetCalls++
		assertions = append(assertions, r.Header.Get("authorization"))
		require.Equal(t, "account-reset-recovery", r.Header.Get("chatgpt-account-id"))
		if resetCalls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_task_id"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":"ok","windows_reset":2}`))
	}))
	defer srv.Close()
	oldBase := openAIAgentIdentityAuthAPIBaseURL
	openAIAgentIdentityAuthAPIBaseURL = srv.URL
	t.Cleanup(func() { openAIAgentIdentityAuthAPIBaseURL = oldBase })

	invalidator := &agentIdentityWSInvalidationRecorder{}
	svc := NewOpenAIQuotaService(repo, nil, nil, newQuotaRedirectingFactory(srv), nil)
	svc.agentIdentityWS = invalidator

	result, err := svc.ResetCredit(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, "ok", result.Code)
	require.Equal(t, 2, result.WindowsReset)
	require.Equal(t, 2, resetCalls)
	require.Equal(t, 1, registerCalls)
	require.Len(t, assertions, 2)
	require.True(t, strings.HasPrefix(assertions[0], "AgentAssertion "))
	require.True(t, strings.HasPrefix(assertions[1], "AgentAssertion "))
	require.NotEqual(t, assertions[0], assertions[1])
	require.Equal(t, "task-reset-new", account.GetCredential("task_id"))
	require.Equal(t, []int64{account.ID}, invalidator.accountIDs)
}

func TestResetCreditAgentIdentityReusesConcurrentlyRecoveredTask(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	require.NoError(t, err)
	account := &Account{
		ID:       202,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"auth_mode":          OpenAIAuthModeAgentIdentity,
			"agent_runtime_id":   "runtime-reset-concurrent",
			"agent_private_key":  base64.StdEncoding.EncodeToString(der),
			"task_id":            "task-reset-old",
			"chatgpt_account_id": "account-reset-concurrent",
		},
	}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	resetCalls := 0
	registerCalls := 0
	var assertions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if strings.Contains(r.URL.Path, "/task/register") {
			registerCalls++
			_, _ = w.Write([]byte(`{"task_id":"task-reset-unexpected"}`))
			return
		}
		resetCalls++
		assertions = append(assertions, r.Header.Get("authorization"))
		if resetCalls == 1 {
			credentials := shallowCopyMap(account.Credentials)
			credentials["task_id"] = "task-reset-concurrent"
			account.Credentials = credentials
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_task_id"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":"ok","windows_reset":1}`))
	}))
	defer srv.Close()
	oldBase := openAIAgentIdentityAuthAPIBaseURL
	openAIAgentIdentityAuthAPIBaseURL = srv.URL
	t.Cleanup(func() { openAIAgentIdentityAuthAPIBaseURL = oldBase })

	svc := NewOpenAIQuotaService(repo, nil, nil, newQuotaRedirectingFactory(srv), nil)
	result, err := svc.ResetCredit(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, "ok", result.Code)
	require.Equal(t, 2, resetCalls)
	require.Zero(t, registerCalls)
	require.Equal(t, "task-reset-old", decodeAgentAssertionTask(t, assertions[0]))
	require.Equal(t, "task-reset-concurrent", decodeAgentAssertionTask(t, assertions[1]))
}

func TestPrepareUpstreamCallUsesAccountCredentials(t *testing.T) {
	ctx := context.Background()
	account := &Account{
		ID:       100,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
		Credentials: map[string]any{
			"chatgpt_account_id": "account-quota",
		},
	}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{account.ID: account}}

	tokenCache := &stubQuotaTokenCache{tokens: map[string]string{
		OpenAITokenCacheKey(account): "fake-access-token",
	}}
	tokenProvider := NewOpenAITokenProvider(repo, tokenCache, nil)

	// privacyClientFactory 可以是任意合法工厂；prepareUpstreamCall 在返回前不调用它
	svc := NewOpenAIQuotaService(repo, nil, tokenProvider, func(_ string) (*req.Client, error) {
		return req.C(), nil
	}, nil)

	accessToken, chatGPTAccountID, _, _, err := svc.prepareUpstreamCall(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, "fake-access-token", accessToken)
	require.Equal(t, "account-quota", chatGPTAccountID)
}

func TestQueryUsageAgentIdentityUsesAssertionWithoutOAuthToken(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	require.NoError(t, err)
	account := &Account{
		ID:       300,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"auth_mode":                  OpenAIAuthModeAgentIdentity,
			"agent_runtime_id":           "runtime-quota",
			"agent_private_key":          base64.StdEncoding.EncodeToString(der),
			"task_id":                    "task-quota",
			"chatgpt_account_id":         "account-quota",
			"chatgpt_account_is_fedramp": true,
		},
	}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	var authorization string
	var accountHeader string
	var fedrampHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("authorization")
		accountHeader = r.Header.Get("chatgpt-account-id")
		fedrampHeader = r.Header.Get("x-openai-fedramp")
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"allowed":true}}`))
	}))
	defer srv.Close()
	svc := NewOpenAIQuotaService(repo, nil, nil, newQuotaRedirectingFactory(srv), nil)
	usage, err := svc.QueryUsage(context.Background(), account.ID)
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.True(t, strings.HasPrefix(authorization, "AgentAssertion "))
	require.Equal(t, "account-quota", accountHeader)
	require.Equal(t, "true", fedrampHeader)
}

func TestQueryUsageAgentIdentityRecoversInvalidTaskOnce(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	require.NoError(t, err)
	account := &Account{
		ID:       301,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"auth_mode":          OpenAIAuthModeAgentIdentity,
			"agent_runtime_id":   "runtime-quota-recovery",
			"agent_private_key":  base64.StdEncoding.EncodeToString(der),
			"task_id":            "task-quota-old",
			"chatgpt_account_id": "account-quota-recovery",
		},
	}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	usageCalls := 0
	registerCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if strings.Contains(r.URL.Path, "/task/register") {
			registerCalls++
			_, _ = w.Write([]byte(`{"task_id":"task-quota-new"}`))
			return
		}
		if strings.Contains(r.URL.Path, "rate-limit-reset-credits") {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		usageCalls++
		if usageCalls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_task_id"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"allowed":true}}`))
	}))
	defer srv.Close()
	oldBase := openAIAgentIdentityAuthAPIBaseURL
	openAIAgentIdentityAuthAPIBaseURL = srv.URL
	t.Cleanup(func() { openAIAgentIdentityAuthAPIBaseURL = oldBase })

	invalidator := &agentIdentityWSInvalidationRecorder{}
	svc := NewOpenAIQuotaService(repo, nil, nil, newQuotaRedirectingFactory(srv), nil)
	svc.agentIdentityWS = invalidator
	usage, err := svc.QueryUsage(context.Background(), account.ID)
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.Equal(t, 2, usageCalls)
	require.Equal(t, 1, registerCalls)
	require.Equal(t, "task-quota-new", account.GetCredential("task_id"))
	require.Equal(t, []int64{account.ID}, invalidator.accountIDs)
}

func TestParseOpenAIRateLimitResetCreditDetails_CompatibleContainers(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "credits",
			body: `{"credits":[{"id":"secret-id","expires_at":"2026-07-03T04:05:06Z"}]}`,
			want: []string{"2026-07-03T04:05:06Z"},
		},
		{
			name: "rate limit reset credits",
			body: `{"rate_limit_reset_credits":[{"expiresAt":"2026-07-04T04:05:06Z"}]}`,
			want: []string{"2026-07-04T04:05:06Z"},
		},
		{
			name: "items",
			body: `{"items":[{"expires_at":"2026-07-05T04:05:06Z"}]}`,
			want: []string{"2026-07-05T04:05:06Z"},
		},
		{
			name: "data",
			body: `{"data":[{"expires_at":"2026-07-06T04:05:06Z"}]}`,
			want: []string{"2026-07-06T04:05:06Z"},
		},
		{
			name: "array",
			body: `[{"expires_at":"2026-07-07T04:05:06Z"}]`,
			want: []string{"2026-07-07T04:05:06Z"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseOpenAIRateLimitResetCreditDetails([]byte(tt.body))
			require.NoError(t, err)
			require.Len(t, got.Credits, len(tt.want))
			for i := range tt.want {
				require.Equal(t, tt.want[i], got.Credits[i].ExpiresAt)
			}
			encoded, err := json.Marshal(got.Credits)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "secret-id")
		})
	}
}

func TestQueryUsageIncludesResetCreditExpirations_EndToEnd(t *testing.T) {
	ctx := context.Background()
	account := &Account{
		ID:       100,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
		Credentials: map[string]any{
			"chatgpt_account_id": "org-parent123",
		},
	}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{100: account}}
	tokenCache := &stubQuotaTokenCache{tokens: map[string]string{
		OpenAITokenCacheKey(account): "fake-token",
	}}
	tokenProvider := NewOpenAITokenProvider(repo, tokenCache, nil)

	var capturedBeta, capturedOriginator, capturedUA string
	var detailCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			_ = json.NewEncoder(w).Encode(OpenAIQuotaUsage{
				RateLimitResetCredits: &OpenAIRateLimitResetCredits{AvailableCount: 2},
			})
		case "/backend-api/wham/rate-limit-reset-credits":
			detailCalls++
			capturedBeta = r.Header.Get("OpenAI-Beta")
			capturedOriginator = r.Header.Get("originator")
			capturedUA = r.Header.Get("User-Agent")
			require.Equal(t, "org-parent123", r.Header.Get("ChatGPT-Account-ID"))
			_, _ = w.Write([]byte(`{"credits":[{"id":"secret-credit-id","expires_at":"2026-07-03T04:05:06Z"},{"expiresAt":"2026-07-04T04:05:06Z"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	svc := NewOpenAIQuotaService(repo, nil, tokenProvider, newQuotaRedirectingFactory(srv), nil)
	usage, err := svc.QueryUsage(ctx, 100)
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.NotNil(t, usage.RateLimitResetCredits)
	require.Equal(t, 2, usage.RateLimitResetCredits.AvailableCount)
	require.Equal(t, 1, detailCalls)
	// Codex backend-client headers: no OpenAI-Beta/originator, canonical Codex UA.
	require.Empty(t, capturedBeta)
	require.Empty(t, capturedOriginator)
	require.Equal(t, CodexCanonicalUserAgent(), capturedUA)
	require.Equal(t, []OpenAIRateLimitResetCreditDetail{
		{ExpiresAt: "2026-07-03T04:05:06Z"},
		{ExpiresAt: "2026-07-04T04:05:06Z"},
	}, usage.RateLimitResetCredits.Credits)
	require.NoError(t, svc.CacheResetCreditsSnapshot(ctx, 100, usage.RateLimitResetCredits))
	require.Equal(t, &OpenAIRateLimitResetCredits{
		AvailableCount: 2,
		Credits: []OpenAIRateLimitResetCreditDetail{
			{ExpiresAt: "2026-07-03T04:05:06Z"},
			{ExpiresAt: "2026-07-04T04:05:06Z"},
		},
	}, repo.extraUpdates[100][openaiQuotaResetCreditsKey])

	encoded, err := json.Marshal(usage)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "secret-credit-id")
}

func TestQueryUsageResetCreditDetails401NonFatal(t *testing.T) {
	ctx := context.Background()
	account := &Account{
		ID:       100,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
		Credentials: map[string]any{
			"chatgpt_account_id": "org-parent123",
		},
	}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{100: account}}
	tokenCache := &stubQuotaTokenCache{tokens: map[string]string{
		OpenAITokenCacheKey(account): "fake-token",
	}}
	tokenProvider := NewOpenAITokenProvider(repo, tokenCache, nil)

	var detailCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			_ = json.NewEncoder(w).Encode(OpenAIQuotaUsage{
				RateLimitResetCredits: &OpenAIRateLimitResetCredits{AvailableCount: 1},
			})
		case "/backend-api/wham/rate-limit-reset-credits":
			detailCalls++
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized","id":"secret-error-id"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	svc := NewOpenAIQuotaService(repo, nil, tokenProvider, newQuotaRedirectingFactory(srv), nil)
	usage, err := svc.QueryUsage(ctx, 100)
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.NotNil(t, usage.RateLimitResetCredits)
	require.Equal(t, 1, usage.RateLimitResetCredits.AvailableCount)
	require.Equal(t, 1, detailCalls)
	require.Empty(t, usage.RateLimitResetCredits.Credits)

	// A count without expiration details must not be persisted (the reader could
	// never age it out), and the previous snapshot must survive untouched.
	require.Error(t, svc.CacheResetCreditsSnapshot(ctx, 100, usage.RateLimitResetCredits))
	require.Empty(t, repo.extraUpdates)
}

func TestCacheResetCreditsSnapshot(t *testing.T) {
	ctx := context.Background()

	t.Run("zero count allows an empty expiration list", func(t *testing.T) {
		repo := &stubQuotaAccountRepo{}
		svc := &OpenAIQuotaService{accountRepo: repo}
		credits := &OpenAIRateLimitResetCredits{AvailableCount: 0}

		require.NoError(t, svc.CacheResetCreditsSnapshot(ctx, 100, credits))
		require.Equal(t, credits, repo.extraUpdates[100][openaiQuotaResetCreditsKey])
	})

	t.Run("missing expiration list preserves the cache", func(t *testing.T) {
		repo := &stubQuotaAccountRepo{}
		svc := &OpenAIQuotaService{accountRepo: repo}

		err := svc.CacheResetCreditsSnapshot(ctx, 100, &OpenAIRateLimitResetCredits{AvailableCount: 1})

		require.Error(t, err)
		require.Empty(t, repo.extraUpdates)
	})

	t.Run("empty expiration list with a positive count preserves the cache", func(t *testing.T) {
		repo := &stubQuotaAccountRepo{}
		svc := &OpenAIQuotaService{accountRepo: repo}

		err := svc.CacheResetCreditsSnapshot(ctx, 100, &OpenAIRateLimitResetCredits{
			AvailableCount: 2,
			Credits:        []OpenAIRateLimitResetCreditDetail{},
		})

		require.Error(t, err)
		require.Empty(t, repo.extraUpdates)
	})

	t.Run("nil snapshot preserves the cache", func(t *testing.T) {
		repo := &stubQuotaAccountRepo{}
		svc := &OpenAIQuotaService{accountRepo: repo}

		require.Error(t, svc.CacheResetCreditsSnapshot(ctx, 100, nil))
		require.Empty(t, repo.extraUpdates)
	})

	t.Run("repository errors are returned", func(t *testing.T) {
		repo := &stubQuotaAccountRepo{extraUpdateErr: errors.New("database unavailable")}
		svc := &OpenAIQuotaService{accountRepo: repo}

		err := svc.CacheResetCreditsSnapshot(ctx, 100, &OpenAIRateLimitResetCredits{
			AvailableCount: 1,
			Credits:        []OpenAIRateLimitResetCreditDetail{{ExpiresAt: "2026-07-03T04:05:06Z"}},
		})

		require.ErrorContains(t, err, "database unavailable")
	})
}

func TestCachePostResetSnapshot(t *testing.T) {
	repo := &stubQuotaAccountRepo{}
	svc := &OpenAIQuotaService{accountRepo: repo}
	credits := &OpenAIRateLimitResetCredits{AvailableCount: 0}
	balance := "1200.50"
	usage := &OpenAIQuotaUsage{
		Credits:               &OpenAICredits{HasCredits: true, Balance: &balance},
		FetchedAt:             123,
		RateLimitResetCredits: credits,
		RateLimit: &OpenAIRateLimit{
			PrimaryWindow: &OpenAIRateLimitWindow{
				UsedPercent: 0, LimitWindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 5 * 60 * 60,
			},
			SecondaryWindow: &OpenAIRateLimitWindow{
				UsedPercent: 0, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAfterSeconds: 7 * 24 * 60 * 60,
			},
		},
	}

	require.NoError(t, svc.CachePostResetSnapshot(context.Background(), 100, usage))
	require.Equal(t, 1, repo.extraUpdateCalls)
	require.Equal(t, credits, repo.extraUpdates[100][openaiQuotaResetCreditsKey])
	require.Equal(t, openAICreditsSnapshot{Credits: usage.Credits, FetchedAt: 123}, repo.extraUpdates[100][openaiQuotaCreditsKey])
	require.Equal(t, 0.0, repo.extraUpdates[100]["codex_5h_used_percent"])
	require.Equal(t, 0.0, repo.extraUpdates[100]["codex_7d_used_percent"])
}

func TestResetCreditValidationDoesNotContactUpstream(t *testing.T) {
	tests := []struct {
		name     string
		accounts map[int64]*Account
		code     int
		reason   string
	}{
		{name: "lookup error", code: http.StatusNotFound, reason: "OPENAI_QUOTA_ACCOUNT_NOT_FOUND"},
		{name: "nil account", accounts: map[int64]*Account{200: nil}, code: http.StatusNotFound, reason: "OPENAI_QUOTA_ACCOUNT_NOT_FOUND"},
		{name: "invalid platform", accounts: map[int64]*Account{200: {ID: 200, Platform: PlatformAnthropic, Type: AccountTypeOAuth}}, code: http.StatusBadRequest, reason: "OPENAI_QUOTA_INVALID_PLATFORM"},
		{name: "invalid type", accounts: map[int64]*Account{200: {ID: 200, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}}, code: http.StatusBadRequest, reason: "OPENAI_QUOTA_INVALID_TYPE"},
		{name: "missing account id", accounts: map[int64]*Account{200: {ID: 200, Platform: PlatformOpenAI, Type: AccountTypeOAuth}}, code: http.StatusBadRequest, reason: "OPENAI_QUOTA_MISSING_ACCOUNT_ID"},
		{name: "missing token provider", accounts: map[int64]*Account{200: {ID: 200, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "account-quota"}}}, code: http.StatusInternalServerError, reason: "OPENAI_QUOTA_NOT_CONFIGURED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &stubQuotaAccountRepo{accounts: tt.accounts}
			clientCalls := 0
			svc := NewOpenAIQuotaService(repo, nil, nil, func(_ string) (*req.Client, error) {
				clientCalls++
				return nil, errors.New("unexpected upstream client creation")
			}, nil)

			result, err := svc.ResetCredit(context.Background(), 200)
			require.Nil(t, result)
			require.Equal(t, tt.code, infraerrors.Code(err))
			require.Equal(t, tt.reason, infraerrors.Reason(err))
			result, err = svc.ResetCreditTargeted(context.Background(), 200, "credit-123", "redeem-456")
			require.Nil(t, result)
			require.Equal(t, tt.code, infraerrors.Code(err))
			require.Equal(t, tt.reason, infraerrors.Reason(err))
			require.Zero(t, clientCalls)
		})
	}
}

func TestResetCreditUnconfigured(t *testing.T) {
	for _, svc := range []*OpenAIQuotaService{nil, {}, {accountRepo: &stubQuotaAccountRepo{}}} {
		result, err := svc.ResetCredit(context.Background(), 200)
		require.Nil(t, result)
		require.Equal(t, http.StatusInternalServerError, infraerrors.Code(err))
		require.Equal(t, "OPENAI_QUOTA_NOT_CONFIGURED", infraerrors.Reason(err))
	}
}

func TestResetCreditTargetedRequiresBothIDs(t *testing.T) {
	svc := &OpenAIQuotaService{}
	for _, ids := range [][2]string{{" ", "redeem-456"}, {"credit-123", " "}} {
		result, err := svc.ResetCreditTargeted(context.Background(), 200, ids[0], ids[1])
		require.Nil(t, result)
		require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
		require.Equal(t, "OPENAI_QUOTA_TARGETED_RESET_INVALID", infraerrors.Reason(err))
	}
}

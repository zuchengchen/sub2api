package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestQueryUsageCodexCredits(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"decimal balance", `{"credits":{"has_credits":true,"unlimited":false,"balance":"12345678901234567890.0123"}}`, `{"has_credits":true,"unlimited":false,"balance":"12345678901234567890.0123"}`},
		{"zero", `{"credits":{"has_credits":false,"unlimited":false,"balance":"0"}}`, `{"has_credits":false,"unlimited":false,"balance":"0"}`},
		{"unlimited", `{"credits":{"has_credits":false,"unlimited":true,"balance":null}}`, `{"has_credits":false,"unlimited":true,"balance":null}`},
		{"hidden balance", `{"credits":{"has_credits":true,"unlimited":false}}`, `{"has_credits":true,"unlimited":false,"balance":null}`},
		{"absent", `{}`, `null`},
		{"null", `{"credits":null}`, `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{ID: 100, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
				Credentials: map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-workspace"}}
			repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{100: account}}
			tokens := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(account): "test-token"}}
			var paths []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				require.Equal(t, http.MethodGet, r.Method)
				require.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
				require.Equal(t, "test-workspace", r.Header.Get("ChatGPT-Account-ID"))
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/backend-api/wham/usage" {
					_, _ = w.Write([]byte(tc.body))
					return
				}
				// Reset-card details may be unavailable without losing points.
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			}))
			defer srv.Close()
			svc := NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, tokens, nil), newQuotaRedirectingFactory(srv), nil)
			usage, err := svc.QueryUsage(context.Background(), 100)
			require.NoError(t, err)
			require.Positive(t, usage.FetchedAt)
			encoded, err := json.Marshal(usage.Credits)
			require.NoError(t, err)
			require.JSONEq(t, tc.want, string(encoded))
			require.Equal(t, []string{"/backend-api/wham/usage", "/backend-api/wham/rate-limit-reset-credits"}, paths)
			require.Zero(t, repo.extraUpdateCalls, "read-only query must not write account state")
		})
	}
}

func TestRefreshCreditSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name          string
		usageStatus   int
		detailsStatus int
	}{
		{"both snapshots share one write", http.StatusOK, http.StatusOK},
		{"points survive unavailable card details", http.StatusOK, http.StatusServiceUnavailable},
		{"failed query preserves both snapshots", http.StatusServiceUnavailable, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
				Credentials: map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-workspace"}}
			repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{account.ID: account}}
			tokens := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(account): "test-token"}}
			var requests []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.Method+" "+r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/backend-api/wham/usage":
					w.WriteHeader(tc.usageStatus)
					_, _ = w.Write([]byte(`{"credits":{"has_credits":true,"balance":"23.75"},"rate_limit_reset_credits":{"available_count":1}}`))
				case "/backend-api/wham/rate-limit-reset-credits":
					w.WriteHeader(tc.detailsStatus)
					_, _ = w.Write([]byte(`{"available_count":1,"credits":[{"id":"private-card","expires_at":"2028-01-01T00:00:00Z"}]}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			svc := NewOpenAIQuotaService(repo, nil, NewOpenAITokenProvider(repo, tokens, nil), newQuotaRedirectingFactory(srv), nil)
			before := time.Now().UnixMicro()
			err := svc.RefreshCreditSnapshots(context.Background(), account.ID)
			if tc.usageStatus != http.StatusOK {
				require.Error(t, err)
				require.Equal(t, []string{"GET /backend-api/wham/usage"}, requests)
				require.Zero(t, repo.extraUpdateCalls)
				return
			}
			require.Equal(t, []string{"GET /backend-api/wham/usage", "GET /backend-api/wham/rate-limit-reset-credits"}, requests)
			require.Equal(t, 1, repo.extraUpdateCalls)
			updates := repo.extraUpdates[account.ID]
			points := updates[openaiQuotaCreditsKey].(openAICreditsSnapshot)
			require.Equal(t, "23.75", *points.Credits.Balance)
			require.GreaterOrEqual(t, points.QueryStartedAt, before)
			if tc.detailsStatus != http.StatusOK {
				require.Error(t, err)
				require.Len(t, updates, 1)
				return
			}
			require.NoError(t, err)
			require.Len(t, updates, 2)
			cards := updates[openaiQuotaResetCreditsKey].(*OpenAIRateLimitResetCredits)
			require.Equal(t, 1, cards.AvailableCount)
			require.Equal(t, "2028-01-01T00:00:00Z", cards.Credits[0].ExpiresAt)
			require.Equal(t, points.FetchedAt, cards.FetchedAt)
			require.Equal(t, points.QueryStartedAt, cards.QueryStartedAt)
			raw, err := json.Marshal(updates)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "private-card")
		})
	}
}

func TestCacheCodexCreditsSnapshot(t *testing.T) {
	ctx := context.Background()
	repo := &stubQuotaAccountRepo{}
	svc := &OpenAIQuotaService{accountRepo: repo}
	balance := "1200.50"
	usage := &OpenAIQuotaUsage{FetchedAt: 123, Credits: &OpenAICredits{HasCredits: true, Balance: &balance}}

	// No reset-card details are required to persist the balance on the queried row.
	require.NoError(t, svc.CacheCreditsSnapshot(ctx, 200, usage))
	encoded, err := json.Marshal(repo.extraUpdates[200][openaiQuotaCreditsKey])
	require.NoError(t, err)
	require.JSONEq(t, `{"credits":{"has_credits":true,"unlimited":false,"balance":"1200.50"},"fetched_at":123}`, string(encoded))
	require.NotContains(t, repo.extraUpdates, int64(100), "a shadow row must not overwrite its parent")
	require.NotContains(t, repo.extraUpdates[200], openaiQuotaResetCreditsKey)

	// Missing credits from a successful read invalidate the previous balance.
	require.NoError(t, svc.CacheCreditsSnapshot(ctx, 200, &OpenAIQuotaUsage{FetchedAt: 456}))
	encoded, err = json.Marshal(repo.extraUpdates[200][openaiQuotaCreditsKey])
	require.NoError(t, err)
	require.JSONEq(t, `{"credits":null,"fetched_at":456}`, string(encoded))

	require.Error(t, svc.CacheCreditsSnapshot(ctx, 200, nil))
	require.Equal(t, 2, repo.extraUpdateCalls)
	repo.extraUpdateErr = errors.New("database unavailable")
	require.ErrorContains(t, svc.CacheCreditsSnapshot(ctx, 200, usage), "database unavailable")
}

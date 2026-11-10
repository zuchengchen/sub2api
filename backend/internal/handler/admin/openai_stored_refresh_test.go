//go:build unit

package admin

import (
	"context"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type storedOpenAIRefreshTestRepo struct {
	service.AccountRepository
	account  *service.Account
	casCalls int
}

func (r *storedOpenAIRefreshTestRepo) GetByID(context.Context, int64) (*service.Account, error) {
	copy := *r.account
	copy.Credentials = service.MergeCredentials(nil, r.account.Credentials)
	return &copy, nil
}
func (r *storedOpenAIRefreshTestRepo) UpdateOpenAIOAuthCredentialsIfUnchanged(_ context.Context, id int64, expected map[string]any, proxy *int64, credentials map[string]any) (bool, error) {
	r.casCalls++
	if r.account.ID != id || !reflect.DeepEqual(r.account.Credentials, expected) || !reflect.DeepEqual(r.account.ProxyID, proxy) {
		return false, nil
	}
	r.account.Credentials = service.MergeCredentials(nil, credentials)
	return true, nil
}

type storedOpenAIRefreshTestExecutor struct {
	repo  *storedOpenAIRefreshTestRepo
	calls int
}

func (e *storedOpenAIRefreshTestExecutor) CanRefresh(*service.Account) bool { return true }
func (e *storedOpenAIRefreshTestExecutor) NeedsRefresh(*service.Account, time.Duration) bool {
	return true
}
func (e *storedOpenAIRefreshTestExecutor) CacheKey(a *service.Account) string {
	return service.OpenAITokenCacheKey(a)
}
func (e *storedOpenAIRefreshTestExecutor) Refresh(_ context.Context, a *service.Account) (map[string]any, error) {
	e.calls++
	result := service.MergeCredentials(nil, a.Credentials)
	result["access_token"] = "obsolete-provider-access"
	result["refresh_token"] = "obsolete-provider-refresh"
	// Concurrent administrator replaces credentials for the same user while the
	// provider response is in flight. The winner must survive both HTTP handlers.
	e.repo.account.Credentials = service.MergeCredentials(nil, a.Credentials)
	e.repo.account.Credentials["access_token"] = "admin-access"
	e.repo.account.Credentials["refresh_token"] = "admin-refresh"
	return result, nil
}

func TestOpenAIStoredRefreshHandlersUseSharedSuccessCAS(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"accounts", "openai"} {
		t.Run(endpoint, func(t *testing.T) {
			account := &service.Account{ID: 42, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Credentials: map[string]any{"access_token": "observed", "refresh_token": "old-refresh", "chatgpt_account_id": "workspace", "chatgpt_user_id": "user"}}
			durable := *account
			durable.Credentials = service.MergeCredentials(nil, account.Credentials)
			repo := &storedOpenAIRefreshTestRepo{account: &durable}
			executor := &storedOpenAIRefreshTestExecutor{repo: repo}
			oauth := service.NewOpenAIOAuthService(nil, nil)
			oauth.SetStoredAccountRefresh(service.NewOAuthRefreshAPI(repo, nil), executor)
			admin := newStubAdminService()
			admin.getAccountResult = account
			if endpoint == "accounts" {
				h := &AccountHandler{adminService: admin, openaiOAuthService: oauth}
				got, _, err := h.refreshSingleAccount(context.Background(), account)
				require.NoError(t, err)
				require.Equal(t, "admin-refresh", got.GetOpenAIRefreshToken())
			} else {
				h := &OpenAIOAuthHandler{adminService: admin, openaiOAuthService: oauth}
				router := gin.New()
				router.POST("/api/v1/admin/openai/accounts/:id/refresh", h.RefreshAccountToken)
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/admin/openai/accounts/42/refresh", nil))
				require.Equal(t, 200, w.Code, w.Body.String())
			}
			require.Equal(t, 1, executor.calls)
			require.Equal(t, 1, repo.casCalls)
			require.Equal(t, "admin-refresh", repo.account.GetOpenAIRefreshToken())
			require.Zero(t, admin.updateAccountCalls)
		})
	}
}

//go:build unit

package admin

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type scopedStatisticsAccounts struct {
	service.AdminService
	accounts map[int64]*service.Account
}

func (s *scopedStatisticsAccounts) GetAccount(_ context.Context, id int64) (*service.Account, error) {
	return s.accounts[id], nil
}
func statisticsPolicy(permission string) adminaccess.Policy {
	return adminaccess.Policy{Rules: []adminaccess.Rule{{Permissions: []string{permission}, GroupIDs: []int64{1}, AccountTypes: []string{"oauth"}, Platforms: []string{"openai"}}}}
}
func statisticsContext(c *gin.Context, permission string) {
	c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), &adminaccess.Principal{Policy: statisticsPolicy(permission)}))
}

func TestScopedAdminOpenAIRejectsBeforeUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &scopedStatisticsAccounts{accounts: map[int64]*service.Account{1: {ID: 1, Platform: "openai", Type: "oauth", GroupIDs: []int64{1}}}}
	stub := &referralHandlerStub{}
	h := &OpenAIOAuthHandler{adminService: svc, referralService: stub}
	r := gin.New()
	r.Use(func(c *gin.Context) { statisticsContext(c, "accounts.read") })
	r.POST("/:id/referrals/invite", h.SendReferralInvite)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/1/referrals/invite", strings.NewReader(`{"email":"friend@example.com","program_id":"codex_referral_consumer","confirmed":true}`)))
	require.Equal(t, 404, w.Code, w.Body.String())
	require.Zero(t, stub.sends)
}

func TestScopedAdminOpenAIQuotaActionsAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		action, path, method string
		allow                bool
	}{
		{"openai.quota.read", "/1/quota", "GET", true}, {"openai.quota.read", "/1/quota/refresh", "POST", true},
		{"accounts.read", "/1/quota", "GET", false}, {"openai.quota.reset", "/1/quota", "GET", false},
	} {
		t.Run(tc.action+tc.path, func(t *testing.T) {
			svc := &scopedStatisticsAccounts{accounts: map[int64]*service.Account{1: {ID: 1, Platform: "openai", Type: "oauth", GroupIDs: []int64{1}}}}
			stub := &openAIQuotaWorkflowStub{queryResult: &service.OpenAIQuotaUsage{UserID: "IDENTITY_SECRET", Email: "IDENTITY_SECRET", AccountID: "IDENTITY_SECRET", PlanType: "IDENTITY_SECRET", RateLimit: &service.OpenAIRateLimit{PrimaryWindow: &service.OpenAIRateLimitWindow{UsedPercent: 23}}, RateLimitResetCredits: &service.OpenAIRateLimitResetCredits{AvailableCount: 3}}}
			h := &OpenAIOAuthHandler{adminService: svc, quotaService: stub}
			r := gin.New()
			r.Use(func(c *gin.Context) { statisticsContext(c, tc.action) })
			r.GET("/:id/quota", h.QueryQuota)
			r.POST("/:id/quota/refresh", h.RefreshQuota)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			if tc.allow {
				require.Equal(t, 200, w.Code, w.Body.String())
				require.Equal(t, 1, stub.queryCalls)
				require.Contains(t, w.Body.String(), `"available_count":3`)
				require.Contains(t, w.Body.String(), `"used_percent":23`)
			} else {
				require.Equal(t, 404, w.Code)
				require.Zero(t, stub.queryCalls)
				require.Zero(t, stub.cacheCalls)
			}
			require.NotContains(t, w.Body.String(), "SECRET")
		})
	}
}

func TestScopedAdminMutationResponseRequiresReadPermission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, read := range []bool{false, true} {
		for _, kind := range []string{"reset", "invite"} {
			t.Run(fmt.Sprintf("%s/read=%v", kind, read), func(t *testing.T) {
				count := 2
				svc := &scopedStatisticsAccounts{accounts: map[int64]*service.Account{1: {ID: 1, Platform: "openai", Type: "oauth", GroupIDs: []int64{1}}}}
				quota := &openAIQuotaWorkflowStub{resetResult: &service.OpenAIQuotaResetResult{Code: "success"}, queryResult: &service.OpenAIQuotaUsage{RateLimitResetCredits: &service.OpenAIRateLimitResetCredits{AvailableCount: 3}, Email: "IDENTITY_SECRET"}}
				referral := &referralHandlerStub{eligibility: &service.OpenAIReferralEligibility{ShouldShow: true, AvailableInvites: &count}}
				h := &OpenAIOAuthHandler{adminService: svc, quotaService: quota, referralService: referral, rateLimitService: &openAIAccountStateRecovererStub{}}
				action, readAction, path := "openai.quota.reset", "openai.quota.read", "/1/reset"
				if kind == "invite" {
					action, readAction, path = "openai.referrals.invite", "openai.referrals.read", "/1/invite"
				}
				r := gin.New()
				r.Use(func(c *gin.Context) {
					statisticsContext(c, action)
					if read {
						p := adminaccess.FromContext(c.Request.Context())
						p.Policy.Rules[0].Permissions = append(p.Policy.Rules[0].Permissions, readAction)
					}
				})
				r.POST("/:id/reset", h.ResetQuota)
				r.POST("/:id/invite", h.SendReferralInvite)
				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(`{"email":"friend@example.com","program_id":"codex_referral_consumer","confirmed":true}`)))
				require.Equal(t, 200, w.Code, w.Body.String())
				require.NotContains(t, w.Body.String(), "IDENTITY_SECRET")
				if kind == "reset" {
					require.Equal(t, 1, quota.resetCalls)
					if read {
						require.Contains(t, w.Body.String(), "available_count")
					} else {
						require.NotContains(t, w.Body.String(), "available_count")
					}
				} else {
					require.Equal(t, 1, referral.sends)
					if read {
						require.Contains(t, w.Body.String(), "available_invites")
					} else {
						require.NotContains(t, w.Body.String(), "available_invites")
					}
				}
			})
		}
	}
}

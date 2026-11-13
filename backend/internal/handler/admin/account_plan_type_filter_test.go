package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountHandlerListPassesPlanTypeToListAndSchedulerPool(t *testing.T) {
	for _, lite := range []string{"0", "1"} {
		t.Run("lite="+lite, func(t *testing.T) {
			router, adminSvc := setupAccountListRouter()
			adminSvc.accounts[0].Platform = service.PlatformOpenAI
			adminSvc.accounts[0].Credentials = map[string]any{"plan_type": "prolite"}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts?plan_type=%20prolite%20&include_scheduler_score=1&lite="+lite, nil)
			router.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, "prolite", adminSvc.lastListAccounts.planType)
			require.Equal(t, 1, adminSvc.schedulerScoreFilterCalls)
			require.Equal(t, "prolite", adminSvc.schedulerScoreFilterPlanType)
		})
	}
}

func TestAccountHandlerBillingRatesPassesPlanType(t *testing.T) {
	router, adminSvc := setupAccountListRouter()
	handler := NewAccountHandler(adminSvc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router.GET("/api/v1/admin/accounts/upstream-billing-rates", handler.GetUpstreamBillingRates)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/upstream-billing-rates?plan_type=promax&platform=openai&group=ungrouped", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "promax", adminSvc.lastListAccounts.planType)
	require.Equal(t, "openai", adminSvc.lastListAccounts.platform)
	require.Equal(t, service.AccountListGroupUngrouped, adminSvc.lastListAccounts.groupID)
}

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type performanceListWindowRepo struct {
	service.UsageLogRepository
	windows []service.AccountWindowQuery
	calls   int
	fail    bool
}

func (r *performanceListWindowRepo) GetAccountWindowStatsForAccounts(_ context.Context, windows []service.AccountWindowQuery) (map[int64]service.AccountWindowStats, error) {
	r.calls++
	r.windows = windows
	if r.fail {
		return nil, errors.New("batch failed")
	}
	result := make(map[int64]service.AccountWindowStats)
	for _, w := range windows {
		result[w.AccountID] = service.AccountWindowStats{Primary: &usagestats.AccountStats{Cost: 99, StandardCost: float64(w.AccountID)}}
	}
	return result, nil
}
func (r *performanceListWindowRepo) GetAccountWindowStats(_ context.Context, id int64, _ time.Time) (*usagestats.AccountStats, error) {
	if id == 2 {
		return nil, errors.New("account window unavailable")
	}
	return &usagestats.AccountStats{Cost: 99, StandardCost: float64(id)}, nil
}

func TestPerformanceOptimizationAccountListWindowStats(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "batch", true: "partial_failure"}[fail], func(t *testing.T) {
			repo := &performanceListWindowRepo{fail: fail}
			usage := service.NewAccountUsageService(nil, repo, nil, nil, nil, nil, service.NewUsageCache(), nil, nil)
			adminSvc := newStubAdminService()
			now := time.Now().UTC()
			start1, start2, end := now.Add(-time.Hour), now.Add(-2*time.Hour), now.Add(time.Hour)
			adminSvc.accounts = []service.Account{
				{ID: 1, Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth, Extra: map[string]any{"window_cost_limit": 100}, SessionWindowStart: &start1, SessionWindowEnd: &end},
				{ID: 2, Platform: service.PlatformAnthropic, Type: service.AccountTypeSetupToken, Extra: map[string]any{"window_cost_limit": 100}, SessionWindowStart: &start2, SessionWindowEnd: &end},
				{ID: 3, Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth},
				{ID: 4, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Extra: map[string]any{"window_cost_limit": 100}},
			}
			handler := NewAccountHandler(adminSvc, nil, nil, nil, nil, usage, nil, nil, nil, nil, nil, nil)
			router := gin.New()
			router.GET("/accounts", handler.List)
			record := httptest.NewRecorder()
			router.ServeHTTP(record, httptest.NewRequest(http.MethodGet, "/accounts?lite=1", nil))
			require.Equal(t, http.StatusOK, record.Code)
			require.Equal(t, 1, repo.calls)
			require.Equal(t, []service.AccountWindowQuery{{AccountID: 1, StartTime: start1}, {AccountID: 2, StartTime: start2}}, repo.windows)
			var payload struct {
				Data struct {
					Items []struct {
						ID   int64    `json:"id"`
						Cost *float64 `json:"current_window_cost"`
					} `json:"items"`
				} `json:"data"`
			}
			require.NoError(t, json.Unmarshal(record.Body.Bytes(), &payload))
			require.Len(t, payload.Data.Items, 4)
			for _, item := range payload.Data.Items {
				if item.ID == 1 || (item.ID == 2 && !fail) {
					require.NotNil(t, item.Cost)
					require.Equal(t, float64(item.ID), *item.Cost, "list keeps standard cost, not multiplied cost")
				} else {
					require.Nil(t, item.Cost, "unknown or ineligible cost must not be zero")
				}
			}
			refresh := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/accounts?lite=1", nil)
			request.Header.Set("If-None-Match", record.Header().Get("ETag"))
			router.ServeHTTP(refresh, request)
			require.Equal(t, http.StatusNotModified, refresh.Code)
			require.Equal(t, 2, repo.calls, "list must retain fresh DB reads on refresh")
		})
	}
}

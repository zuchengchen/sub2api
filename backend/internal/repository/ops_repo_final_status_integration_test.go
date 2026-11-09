//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpsErrorFinalStatusPreservesRequestAndAttempt(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	owner := mustCreateUser(t, client, &service.User{})
	foreign := mustCreateUser(t, client, &service.User{})
	repo := NewOpsRepository(integrationDB)
	model := fmt.Sprintf("final-status-audit-%d", owner.ID)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM ops_error_logs WHERE model = $1", model)
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id IN ($1, $2)", owner.ID, foreign.ID)
	})
	upstream := 503
	insert := func(uid int64, status int) int64 {
		id, err := repo.InsertErrorLog(ctx, &service.OpsInsertErrorLogInput{
			UserID: &uid, Model: model, RequestedModel: model, ErrorPhase: "upstream", ErrorType: "upstream_error", Severity: "P2",
			StatusCode: status, UpstreamStatusCode: &upstream, CreatedAt: time.Now(),
		})
		require.NoError(t, err)
		return id
	}
	canceledID := insert(owner.ID, 499)
	failedID := insert(owner.ID, 502)
	recoveredID := insert(owner.ID, 200)
	insert(foreign.ID, 499)
	ops := service.NewOpsService(repo, nil, nil, nil, nil, nil, nil, nil, nil)
	settings := service.NewSettingService(opsCategoryTestSettings{}, nil)
	usageHandler := handler.NewUsageHandler(nil, nil, ops, settings)
	adminHandler := adminhandler.NewOpsHandler(ops)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: owner.ID})
		c.Next()
	})
	router.GET("/usage/errors", usageHandler.ListErrors)
	router.GET("/usage/errors/:id", usageHandler.GetErrorDetail)
	router.GET("/admin/errors", adminHandler.GetErrorLogs)
	router.GET("/admin/request-errors", adminHandler.ListRequestErrors)
	type row struct {
		ID       int64 `json:"id"`
		Status   int   `json:"status_code"`
		Final    *int  `json:"final_request_status"`
		Upstream *int  `json:"upstream_status_code"`
	}
	readList := func(t *testing.T, path string, requestScope bool, status int) []row {
		t.Helper()
		query := url.Values{"model": {model}, "view": {"all"}, "sort_by": {"status_code"}, "sort_order": {"asc"}}
		if path != "/usage/errors" {
			query.Set("user_query", owner.Email)
		}
		if requestScope {
			query.Set("status_scope", "request")
		}
		if status > 0 {
			key := "status_codes"
			if path == "/usage/errors" {
				key = "status_code"
			}
			query.Set(key, strconv.Itoa(status))
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path+"?"+query.Encode(), nil))
		require.Equal(t, 200, rec.Code, rec.Body.String())
		var envelope struct {
			Data struct {
				Items []row `json:"items"`
				Total int   `json:"total"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
		require.Len(t, envelope.Data.Items, envelope.Data.Total)
		return envelope.Data.Items
	}
	for _, path := range []string{"/usage/errors", "/admin/errors", "/admin/request-errors"} {
		t.Run(path+"/request_scope", func(t *testing.T) {
			rows := readList(t, path, true, 499)
			require.Len(t, rows, 1)
			require.Equal(t, canceledID, rows[0].ID)
			require.Equal(t, 503, rows[0].Status, "legacy response field remains upstream-first")
			require.NotNil(t, rows[0].Final)
			require.Equal(t, 499, *rows[0].Final)
			sorted := readList(t, path, true, 0)
			require.Len(t, sorted, 2, "recovered and foreign rows must stay outside the user scope")
			require.Equal(t, []int64{canceledID, failedID}, []int64{sorted[0].ID, sorted[1].ID})
		})
		t.Run(path+"/legacy_scope", func(t *testing.T) {
			rows := readList(t, path, false, 503)
			require.Len(t, rows, 2)
			require.ElementsMatch(t, []int64{canceledID, failedID}, []int64{rows[0].ID, rows[1].ID})
			require.Equal(t, 503, rows[0].Status)
		})
	}
	t.Run("owned_detail_has_both_statuses", func(t *testing.T) {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/usage/errors/%d", canceledID), nil))
		require.Equal(t, 200, rec.Code)
		var envelope struct {
			Data row `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
		require.NotNil(t, envelope.Data.Final)
		require.Equal(t, 499, *envelope.Data.Final)
		require.NotNil(t, envelope.Data.Upstream)
		require.Equal(t, 503, *envelope.Data.Upstream)
		require.NotContains(t, rec.Body.String(), `"account_id"`)
		require.NotContains(t, rec.Body.String(), `"user_email"`)
	})
	t.Run("provider_health_retains_recovered", func(t *testing.T) {
		provider, err := repo.ListErrorLogs(ctx, &service.OpsErrorLogFilter{
			View: "all", UserID: &owner.ID, Model: model, Phase: "upstream", IncludeRecoveredUpstream: true, StatusCodes: []int{503},
		})
		require.NoError(t, err)
		require.Equal(t, 3, provider.Total)
		var ids []int64
		for _, item := range provider.Errors {
			ids = append(ids, item.ID)
		}
		require.Contains(t, ids, recoveredID)
	})
}

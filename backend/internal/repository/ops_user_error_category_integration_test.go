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

type opsCategoryTestSettings struct{ service.SettingRepository }

func (opsCategoryTestSettings) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{service.SettingKeyAllowUserViewErrorRequests: "true"}, nil
}

func TestOpsUserErrorCategoryFilters(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	owner := mustCreateUser(t, client, &service.User{})
	foreign := mustCreateUser(t, client, &service.User{})
	ownerKey := mustCreateApiKey(t, client, &service.APIKey{UserID: owner.ID})
	foreignKey := mustCreateApiKey(t, client, &service.APIKey{UserID: foreign.ID})
	model := fmt.Sprintf("category-audit-%d", owner.ID)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM ops_error_logs WHERE model = $1", model)
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM api_keys WHERE id IN ($1, $2)", ownerKey.ID, foreignKey.ID)
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id IN ($1, $2)", owner.ID, foreign.ID)
	})

	repo := NewOpsRepository(integrationDB)
	type sample struct {
		phase, errorType, category string
		status                     int
		countTokens                bool
	}
	samples := []sample{
		{"auth", "authentication_error", "auth", 401, false},
		{"routing", "api_error", "service_unavailable", 503, false},
		{"account_auth", "upstream_error", "upstream", 503, false},
		{"upstream", "rate_limit_error", "upstream", 429, false},
		{"upstream", "invalid_request_error", "upstream", 400, false},
		{"upstream", "billing_error", "upstream", 402, false},
		{"network", "api_error", "upstream", 502, false},
		{"internal", "api_error", "internal", 500, false},
		{"internal", "unknown_type", "internal", 500, false},
		{"request", "rate_limit_error", "rate_limit", 429, false},
		{"request", "billing_error", "quota", 402, false},
		{"request", "subscription_error", "quota", 402, false},
		{"request", "invalid_request_error", "invalid_request", 400, false},
		{"request", "cyber_policy", "cyber", 200, false},
		{"request", "api_error", "other", 400, false},
		{"request", "permission_error", "other", 403, false},
		{"request", "not_found_error", "other", 404, false},
		{"request", "unknown_type", "other", 400, false},
		{"unknown_phase", "api_error", "other", 500, false},
		{"request", "api_error", "other", 400, true},
		// These are retained in storage but excluded by the request-error status guard.
		{"upstream", "rate_limit_error", "upstream", 200, false},
		{"request", "api_error", "other", 200, false},
	}
	userExpected := map[string][]int64{}
	adminExpected := map[string][]int64{}
	now := time.Now().UTC()
	for _, fixture := range samples {
		id, err := repo.InsertErrorLog(ctx, &service.OpsInsertErrorLogInput{
			UserID: &owner.ID, APIKeyID: &ownerKey.ID,
			Model: model, RequestedModel: model,
			ErrorPhase: fixture.phase, ErrorType: fixture.errorType,
			Severity: "P2", StatusCode: fixture.status,
			IsBusinessLimited: true, IsCountTokens: fixture.countTokens,
			CreatedAt: now,
		})
		require.NoError(t, err)
		if fixture.status >= 400 || fixture.errorType == "cyber_policy" {
			adminExpected[fixture.category] = append(adminExpected[fixture.category], id)
			adminExpected[""] = append(adminExpected[""], id)
			if !fixture.countTokens {
				userExpected[fixture.category] = append(userExpected[fixture.category], id)
				userExpected[""] = append(userExpected[""], id)
			}
		}
	}
	// A valid foreign key and another user's matching error must not weaken ownership.
	_, err := repo.InsertErrorLog(ctx, &service.OpsInsertErrorLogInput{
		UserID: &foreign.ID, APIKeyID: &foreignKey.ID,
		Model: model, RequestedModel: model,
		ErrorPhase: "request", ErrorType: "api_error", Severity: "P2",
		StatusCode: 400, IsBusinessLimited: true, CreatedAt: now,
	})
	require.NoError(t, err)

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
	router.GET("/admin/errors", adminHandler.GetErrorLogs)
	router.GET("/admin/request-errors", adminHandler.ListRequestErrors)

	assertRequest := func(t *testing.T, path string, values url.Values, expected []int64, wantCategory string) {
		t.Helper()
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path+"?"+values.Encode(), nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var result struct {
			Data struct {
				Total int `json:"total"`
				Items []struct {
					ID       int64  `json:"id"`
					Category string `json:"category"`
				} `json:"items"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
		var ids []int64
		for _, item := range result.Data.Items {
			ids = append(ids, item.ID)
			if path == "/usage/errors" && wantCategory != "" {
				require.Equal(t, wantCategory, item.Category)
			}
		}
		require.Equal(t, len(expected), result.Data.Total)
		require.ElementsMatch(t, expected, ids)
	}
	categories := []string{"auth", "service_unavailable", "upstream", "internal", "rate_limit", "quota", "invalid_request", "cyber", "other", "", "unknown-category"}
	for _, path := range []string{"/usage/errors", "/admin/errors", "/admin/request-errors"} {
		for _, category := range categories {
			t.Run(path+"/"+category, func(t *testing.T) {
				values := url.Values{"model": {model}, "page_size": {"100"}, "view": {"all"}, "category": {category}, "user_id": {strconv.FormatInt(owner.ID, 10)}}
				if path == "/admin/request-errors" {
					// This endpoint exposes user_query, not the user_id filter on /admin/errors.
					values.Set("user_query", owner.Email)
				}
				expected := adminExpected
				if path == "/usage/errors" {
					expected = userExpected
				}
				expectedCategory := category
				if category == "unknown-category" {
					expectedCategory = ""
				}
				assertRequest(t, path, values, expected[expectedCategory], expectedCategory)
			})
		}
	}
	t.Run("user cannot replace authenticated ownership", func(t *testing.T) {
		values := url.Values{"model": {model}, "page_size": {"100"}, "category": {"other"}, "user_id": {strconv.FormatInt(foreign.ID, 10)}}
		assertRequest(t, "/usage/errors", values, userExpected["other"], "other")
		values.Set("api_key_id", strconv.FormatInt(foreignKey.ID, 10))
		assertRequest(t, "/usage/errors", values, nil, "other")
	})
}

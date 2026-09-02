package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func setupAccountMixedChannelRouter(adminSvc *stubAdminService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	accountHandler := NewAccountHandler(adminSvc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router.POST("/api/v1/admin/accounts/check-mixed-channel", accountHandler.CheckMixedChannel)
	router.POST("/api/v1/admin/accounts", accountHandler.Create)
	router.PUT("/api/v1/admin/accounts/:id", accountHandler.Update)
	router.POST("/api/v1/admin/accounts/bulk-update", accountHandler.BulkUpdate)
	return router
}

func TestAccountHandlerCheckMixedChannelNoRisk(t *testing.T) {
	adminSvc := newStubAdminService()
	router := setupAccountMixedChannelRouter(adminSvc)

	body, _ := json.Marshal(map[string]any{
		"platform":  "grok",
		"group_ids": []int64{27},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/check-mixed-channel", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, float64(0), resp["code"])
	data, ok := resp["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, false, data["has_risk"])
	require.Equal(t, int64(0), adminSvc.lastMixedCheck.accountID)
	require.Equal(t, "grok", adminSvc.lastMixedCheck.platform)
	require.Equal(t, []int64{27}, adminSvc.lastMixedCheck.groupIDs)
}

func TestAccountHandlerRejectsRetiredPlatforms(t *testing.T) {
	for _, platform := range []string{"gemini", "antigravity"} {
		t.Run(platform, func(t *testing.T) {
			adminSvc := newStubAdminService()
			router := setupAccountMixedChannelRouter(adminSvc)

			checkBody, _ := json.Marshal(map[string]any{
				"platform":  platform,
				"group_ids": []int64{27},
			})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/check-mixed-channel", bytes.NewReader(checkBody))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Equal(t, "", adminSvc.lastMixedCheck.platform, "已下线平台不应到达 service 层")

			createBody, _ := json.Marshal(map[string]any{
				"name":        "legacy-account",
				"platform":    platform,
				"type":        "oauth",
				"credentials": map[string]any{"refresh_token": "rt"},
			})
			rec = httptest.NewRecorder()
			req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts", bytes.NewReader(createBody))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)
			require.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

func TestAccountHandlerCheckMixedChannelWithRisk(t *testing.T) {
	adminSvc := newStubAdminService()
	adminSvc.checkMixedErr = &service.MixedChannelError{
		GroupID:         27,
		GroupName:       "claude-max",
		CurrentPlatform: "Grok",
		OtherPlatform:   "Anthropic",
	}
	router := setupAccountMixedChannelRouter(adminSvc)

	body, _ := json.Marshal(map[string]any{
		"platform":   "grok",
		"group_ids":  []int64{27},
		"account_id": 99,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/check-mixed-channel", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, float64(0), resp["code"])
	data, ok := resp["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, data["has_risk"])
	require.Equal(t, "mixed_channel_warning", data["error"])
	details, ok := data["details"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(27), details["group_id"])
	require.Equal(t, "claude-max", details["group_name"])
	require.Equal(t, "Grok", details["current_platform"])
	require.Equal(t, "Anthropic", details["other_platform"])
	require.Equal(t, int64(99), adminSvc.lastMixedCheck.accountID)
}

func TestAccountHandlerCreateMixedChannelConflictSimplifiedResponse(t *testing.T) {
	adminSvc := newStubAdminService()
	adminSvc.createAccountErr = &service.MixedChannelError{
		GroupID:         27,
		GroupName:       "claude-max",
		CurrentPlatform: "Grok",
		OtherPlatform:   "Anthropic",
	}
	router := setupAccountMixedChannelRouter(adminSvc)

	body, _ := json.Marshal(map[string]any{
		"name":        "grok-oauth-1",
		"platform":    "grok",
		"type":        "oauth",
		"credentials": map[string]any{"refresh_token": "rt"},
		"group_ids":   []int64{27},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusConflict, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "mixed_channel_warning", resp["error"])
	require.Contains(t, resp["message"], "mixed_channel_warning")
	_, hasDetails := resp["details"]
	_, hasRequireConfirmation := resp["require_confirmation"]
	require.False(t, hasDetails)
	require.False(t, hasRequireConfirmation)
}

func TestAccountHandlerUpdateMixedChannelConflictSimplifiedResponse(t *testing.T) {
	adminSvc := newStubAdminService()
	adminSvc.updateAccountErr = &service.MixedChannelError{
		GroupID:         27,
		GroupName:       "claude-max",
		CurrentPlatform: "Grok",
		OtherPlatform:   "Anthropic",
	}
	router := setupAccountMixedChannelRouter(adminSvc)

	body, _ := json.Marshal(map[string]any{
		"group_ids": []int64{27},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/accounts/3", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusConflict, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "mixed_channel_warning", resp["error"])
	require.Contains(t, resp["message"], "mixed_channel_warning")
	_, hasDetails := resp["details"]
	_, hasRequireConfirmation := resp["require_confirmation"]
	require.False(t, hasDetails)
	require.False(t, hasRequireConfirmation)
}

func TestAccountHandlerUpdateMapsUpstreamBillingRateSyncSettings(t *testing.T) {
	adminSvc := newStubAdminService()
	router := setupAccountMixedChannelRouter(adminSvc)
	body, _ := json.Marshal(map[string]any{
		"name":                               "grok-key",
		"upstream_billing_probe_enabled":     true,
		"upstream_billing_rate_sync_enabled": true,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/accounts/42", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, adminSvc.lastUpdateAccountInput)
	require.NotNil(t, adminSvc.lastUpdateAccountInput.ProbeEnabled)
	require.True(t, *adminSvc.lastUpdateAccountInput.ProbeEnabled)
	require.NotNil(t, adminSvc.lastUpdateAccountInput.RateSyncEnabled)
	require.True(t, *adminSvc.lastUpdateAccountInput.RateSyncEnabled)
}

func TestAccountHandlerBulkUpdateMixedChannelConflict(t *testing.T) {
	adminSvc := newStubAdminService()
	adminSvc.bulkUpdateAccountErr = &service.MixedChannelError{
		GroupID:         27,
		GroupName:       "claude-max",
		CurrentPlatform: "Grok",
		OtherPlatform:   "Anthropic",
	}
	router := setupAccountMixedChannelRouter(adminSvc)

	body, _ := json.Marshal(map[string]any{
		"account_ids": []int64{1, 2, 3},
		"group_ids":   []int64{27},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/bulk-update", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusConflict, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "mixed_channel_warning", resp["error"])
	require.Contains(t, resp["message"], "claude-max")
}

func TestAccountHandlerBulkUpdateMixedChannelConfirmSkips(t *testing.T) {
	adminSvc := newStubAdminService()
	router := setupAccountMixedChannelRouter(adminSvc)

	body, _ := json.Marshal(map[string]any{
		"account_ids":                []int64{1, 2},
		"group_ids":                  []int64{27},
		"confirm_mixed_channel_risk": true,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/bulk-update", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, float64(0), resp["code"])
	data, ok := resp["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, float64(2), data["success"])
	require.Equal(t, float64(0), data["failed"])
}

func TestBulkUpdateAcceptsFilterTargetRequest(t *testing.T) {
	adminSvc := newStubAdminService()
	router := setupAccountMixedChannelRouter(adminSvc)

	body, _ := json.Marshal(map[string]any{
		"filters": map[string]any{
			"platform":     "openai",
			"type":         "oauth",
			"status":       "active",
			"group":        "12",
			"privacy_mode": "blocked",
			"search":       "bulk-target",
		},
		"schedulable": true,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/bulk-update", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, float64(0), resp["code"])
}

func TestBulkUpdateAcceptsDedicatedUpstreamBillingProbeSetting(t *testing.T) {
	adminSvc := newStubAdminService()
	router := setupAccountMixedChannelRouter(adminSvc)

	body, _ := json.Marshal(map[string]any{
		"account_ids":                    []int64{1, 2},
		"upstream_billing_probe_enabled": false,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/bulk-update", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, adminSvc.lastBulkUpdateAccountInput)
	require.NotNil(t, adminSvc.lastBulkUpdateAccountInput.ProbeEnabled)
	require.False(t, *adminSvc.lastBulkUpdateAccountInput.ProbeEnabled)
}

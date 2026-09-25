package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestSettingsExcelBPSImagesRoundTripAndOmission(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{})
	rec := doUpdateSettings(t, h, map[string]any{
		"excel_bps_image_relay_enabled":  true,
		"excel_bps_image_base_url":       " https://images.example/ ",
		"excel_bps_image_body_limit_mib": 32,
		"excel_bps_image_budget_mib":     768,
		"excel_bps_image_max_requests":   48,
	}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, gjson.Get(rec.Body.String(), "data.excel_bps_image_relay_enabled").Bool())
	require.Equal(t, "https://images.example", gjson.Get(rec.Body.String(), "data.excel_bps_image_base_url").String())
	require.Equal(t, "true", repo.values[service.SettingKeyExcelBPSImageRelayEnabled])
	require.Equal(t, "https://images.example", repo.values[service.SettingKeyExcelBPSImageBaseURL])
	require.Equal(t, "32", repo.values[service.SettingKeyExcelBPSImageBodyLimitMiB])
	require.Equal(t, "768", repo.values[service.SettingKeyExcelBPSImageBudgetMiB])
	require.Equal(t, "48", repo.values[service.SettingKeyExcelBPSImageMaxRequests])
	rec = doUpdateSettings(t, h, map[string]any{"site_name": "keep relay"}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)
	h.GetSettings(c)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, gjson.Get(rec.Body.String(), "data.excel_bps_image_relay_enabled").Bool())
	require.Equal(t, "https://images.example", gjson.Get(rec.Body.String(), "data.excel_bps_image_base_url").String())
	require.Equal(t, int64(32), gjson.Get(rec.Body.String(), "data.excel_bps_image_body_limit_mib").Int())
	require.Equal(t, int64(768), gjson.Get(rec.Body.String(), "data.excel_bps_image_budget_mib").Int())
	require.Equal(t, int64(48), gjson.Get(rec.Body.String(), "data.excel_bps_image_max_requests").Int())
	public, err := h.settingService.GetPublicSettings(context.Background())
	require.NoError(t, err)
	encoded, err := json.Marshal(public)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "images.example", "relay configuration is admin-only")
	rec = doUpdateSettings(t, h, map[string]any{"excel_bps_image_base_url": "http://invalid.example"}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Equal(t, "https://images.example", repo.values[service.SettingKeyExcelBPSImageBaseURL])
	for _, field := range []string{"excel_bps_image_body_limit_mib", "excel_bps_image_budget_mib", "excel_bps_image_max_requests"} {
		rec = doUpdateSettings(t, h, map[string]any{field: 0}, nil)
		require.Equal(t, http.StatusBadRequest, rec.Code, field)
	}
	require.Equal(t, "32", repo.values[service.SettingKeyExcelBPSImageBodyLimitMiB])
	rec = doUpdateSettings(t, h, map[string]any{"excel_bps_image_relay_enabled": false}, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "false", repo.values[service.SettingKeyExcelBPSImageRelayEnabled])
	require.Equal(t, "https://images.example", repo.values[service.SettingKeyExcelBPSImageBaseURL])
}

func TestSettingsExcelBPSImagesRequireHTTPSOriginWhenEnabled(t *testing.T) {
	h, _ := newStepUpSwitchTestHandler(t, map[string]string{})
	rec := doUpdateSettings(t, h, map[string]any{"excel_bps_image_relay_enabled": true}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "INVALID_EXCEL_BPS_IMAGE_BASE_URL")
}

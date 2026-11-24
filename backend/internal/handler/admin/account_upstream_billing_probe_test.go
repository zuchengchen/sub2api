package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func setupUpstreamBillingProbeRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	handler := NewAccountHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	handler.SetUpstreamBillingProbeService(service.NewUpstreamBillingProbeService(nil, nil, nil))

	router := gin.New()
	router.GET("/admin/accounts/upstream-billing-probe/settings", handler.GetUpstreamBillingProbeSettings)
	router.POST("/admin/accounts/upstream-billing-probe/batch", handler.ProbeUpstreamBillingBatch)
	router.POST("/admin/accounts/upstream-billing-probe/group-preview", handler.PreviewUpstreamGroup)
	router.POST("/admin/accounts/:id/upstream-billing-probe/group-sync", handler.SyncUpstreamGroup)
	router.PUT("/admin/accounts/:id/upstream-billing-probe", handler.SetUpstreamBillingProbeEnabled)
	return router
}

func TestAccountHandlerUpstreamBillingGroupPreviewKeepsStoredCredentialsBound(t *testing.T) {
	router := setupUpstreamBillingProbeRouter()
	for _, body := range []string{
		`{"account_id":7,"base_url":"https://different.example"}`,
		`{"account_id":7,"proxy_id":8}`,
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin/accounts/upstream-billing-probe/group-preview", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		require.Equal(t, http.StatusBadRequest, w.Code)
	}
}

func TestAccountHandlerUpstreamBillingGroupWritesRequireCurrentVersion(t *testing.T) {
	const path = "/admin/accounts/7/upstream-billing-probe/group-sync"
	gin.SetMode(gin.TestMode)
	router := gin.New()
	h := &AccountHandler{
		adminService: &stubAdminService{getAccountResult: &service.Account{
			ID: 7, UpdatedAt: time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC),
		}},
		upstreamBillingProbe: service.NewUpstreamBillingProbeService(nil, nil, nil),
	}
	router.POST("/admin/accounts/:id/upstream-billing-probe/group-sync", h.SyncUpstreamGroup)
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{}`, http.StatusBadRequest},
		{`{"expected_updated_at":"0001-01-01T00:00:00Z"}`, http.StatusBadRequest},
		{`{"expected_updated_at":"2026-11-24T09:00:00Z"}`, http.StatusConflict},
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(tc.body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		require.Equal(t, tc.want, w.Code, w.Body.String())
	}
}

func TestAccountHandlerUpstreamBillingGroupEndpointsDenyScopedKeys(t *testing.T) {
	router := setupUpstreamBillingProbeRouter()
	for _, path := range []string{
		"/admin/accounts/upstream-billing-probe/group-preview",
		"/admin/accounts/7/upstream-billing-probe/group-sync",
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, nil)
		ctx := adminaccess.WithPrincipal(req.Context(), &adminaccess.Principal{})
		router.ServeHTTP(w, req.WithContext(ctx))
		require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	}
}

func TestAccountHandlerGetUpstreamBillingProbeSettingsReturnsDefaults(t *testing.T) {
	router := setupUpstreamBillingProbeRouter()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/accounts/upstream-billing-probe/settings", nil))

	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Data service.UpstreamBillingProbeSettings `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Data.Enabled)
	require.Equal(t, 30, response.Data.IntervalMinutes)
}

func TestAccountHandlerProbeUpstreamBillingBatchValidatesIDs(t *testing.T) {
	router := setupUpstreamBillingProbeRouter()

	for _, body := range []string{`{"account_ids":[]}`, `{"account_ids":[0]}`} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/admin/accounts/upstream-billing-probe/batch", bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusBadRequest, recorder.Code)
	}
}

func TestAccountHandlerSetUpstreamBillingProbeEnabledRejectsInvalidID(t *testing.T) {
	router := setupUpstreamBillingProbeRouter()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/admin/accounts/not-an-id/upstream-billing-probe", bytes.NewBufferString(`{"enabled":true}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

func TestAccountHandlerSetUpstreamBillingProbeEnabledRequiresValue(t *testing.T) {
	router := setupUpstreamBillingProbeRouter()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/admin/accounts/1/upstream-billing-probe", bytes.NewBufferString(`{}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

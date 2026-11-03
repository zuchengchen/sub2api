package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpsErrorLoggerMiddleware_LocalQuotaCodes(t *testing.T) {
	for _, tc := range []struct {
		name, code, message, wantType string
	}{
		{"balance", "INSUFFICIENT_BALANCE", "Insufficient balance", "billing_error"},
		{"key_quota", "API_KEY_QUOTA_EXHAUSTED", "API key quota exhausted", "billing_error"},
		{"usage_limit", "USAGE_LIMIT_EXCEEDED", "Daily usage limit exceeded", "subscription_error"},
		{"subscription_missing", "SUBSCRIPTION_NOT_FOUND", "No active subscription found for this group", "subscription_error"},
		{"subscription_invalid", "SUBSCRIPTION_INVALID", "Subscription is invalid or expired", "subscription_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupOpsErrorLogTestQueue(t, 2)
			gin.SetMode(gin.TestMode)
			ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil)
			router := gin.New()
			router.Use(OpsErrorLoggerMiddleware(ops))
			router.GET("/v1/models", func(c *gin.Context) {
				middleware2.AbortWithError(c, http.StatusForbidden, tc.code, tc.message)
			})
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
			require.Equal(t, http.StatusForbidden, recorder.Code)
			require.Equal(t, int64(1), OpsErrorLogQueueLength())
			job := <-opsErrorLogQueue
			require.Equal(t, tc.wantType, job.entry.ErrorType)
			require.Equal(t, "request", job.entry.ErrorPhase)
			require.True(t, job.entry.IsBusinessLimited)
			require.Equal(t, "quota", service.MapUserErrorCategory(job.entry.ErrorPhase, job.entry.ErrorType))
		})
	}
}

func TestNormalizeOpsErrorType_GenericCodeFallback(t *testing.T) {
	require.Equal(t, "billing_error", normalizeOpsErrorType("api_error", "INSUFFICIENT_BALANCE"))
	require.Equal(t, "billing_error", normalizeOpsErrorType("api_error", "API_KEY_QUOTA_EXHAUSTED"))
	require.Equal(t, "api_error", normalizeOpsErrorType("api_error", "UNRECOGNIZED"))
	require.Equal(t, "rate_limit_error", normalizeOpsErrorType("rate_limit_error", "INSUFFICIENT_BALANCE"))
	require.Equal(t, "upstream_error", normalizeOpsErrorType("upstream_error", "INSUFFICIENT_BALANCE"))
}

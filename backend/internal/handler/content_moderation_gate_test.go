package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Ported from upstream's websocket security-audit logging fix: dedupe-cache
// hits must still emit an audit log entry (cached=true) instead of returning
// silently.
func TestRunUnifiedContentModerationLogsWebSocketChecksAndCacheHits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := service.NewContentModerationService(nil, nil, nil, nil, nil, nil, nil, nil)
	core, logs := observer.New(zap.InfoLevel)
	reqLog := zap.New(core)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(contentModerationWSTurnContextKey, 2)
	payload := []byte(`{"type":"response.create","response":{"input":"same turn"}}`)

	first := runUnifiedContentModeration(c, reqLog, svc, nil, middleware2.AuthSubject{UserID: 7}, "openai_responses", "gpt-test", payload, "subsequent_turn")
	second := runUnifiedContentModeration(c, reqLog, svc, nil, middleware2.AuthSubject{UserID: 7}, "openai_responses", "gpt-test", payload, "subsequent_turn")
	require.NotNil(t, first)
	require.NotNil(t, second)

	startLogs := logs.FilterMessage("content_moderation.gateway_check_start").All()
	require.Len(t, startLogs, 1)
	require.Equal(t, false, startLogs[0].ContextMap()["cached"])

	doneLogs := logs.FilterMessage("content_moderation.gateway_check_done").All()
	require.Len(t, doneLogs, 2)
	require.Equal(t, false, doneLogs[0].ContextMap()["cached"])
	require.Equal(t, true, doneLogs[1].ContextMap()["cached"])
	require.Equal(t, true, doneLogs[1].ContextMap()["allowed"])
	require.Equal(t, "subsequent_turn", doneLogs[1].ContextMap()["stage"])
}

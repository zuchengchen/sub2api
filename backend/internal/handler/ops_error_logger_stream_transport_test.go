package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpsErrorLogger_UpstreamStreamTransportClassification(t *testing.T) {
	for _, code := range []string{
		service.OpenAIUpstreamStreamReadErrorCode,
		service.OpenAIUpstreamHTTP2StreamErrorCode,
		service.OpenAIUpstreamStreamTruncatedCode,
	} {
		t.Run(code, func(t *testing.T) {
			setupOpsErrorLogTestQueue(t, 2)
			gin.SetMode(gin.TestMode)
			ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil)
			router := gin.New()
			router.Use(OpsErrorLoggerMiddleware(ops))
			router.POST("/responses", func(c *gin.Context) {
				setOpsRequestContext(c, "gpt-5.5", true)
				c.Header("Content-Type", "text/event-stream")
				_, err := c.Writer.WriteString(fmt.Sprintf("event: error\ndata: {\"type\":\"error\",\"code\":%q,\"message\":\"Upstream response stream was interrupted\",\"param\":null,\"sequence_number\":0}\n\n", code))
				require.NoError(t, err)
				service.MarkResponseCommitted(c)
			})
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/responses", nil))
			require.Equal(t, http.StatusOK, recorder.Code)
			require.Equal(t, int64(1), OpsErrorLogQueueLength())
			entry := (<-opsErrorLogQueue).entry
			require.Equal(t, http.StatusBadGateway, entry.StatusCode)
			require.Equal(t, "upstream", entry.ErrorPhase, "known transport error must not appear as an internal product failure")
			require.Equal(t, "provider", entry.ErrorOwner)
			require.Equal(t, "upstream", service.MapUserErrorCategory(entry.ErrorPhase, entry.ErrorType))
		})
	}
}

func TestOpsErrorLogger_TransportCodeClassificationControls(t *testing.T) {
	for _, tc := range []struct{ code, message, expectedPhase string }{
		{"", "Risk-control history check is temporarily unavailable; retry later", "internal"},
		{"unrecognized", "a message mentions upstream_stream_read_error but is not that event", "internal"},
		{"upstream_stream_read_error_custom", "unknown extension", "internal"},
		{"INSUFFICIENT_BALANCE", "Insufficient balance", "request"},
	} {
		t.Run(tc.code+tc.expectedPhase, func(t *testing.T) {
			normalized := normalizeOpsErrorType("api_error", tc.code)
			require.Equal(t, tc.expectedPhase, classifyOpsPhase(normalized, tc.message, tc.code))
		})
	}
}

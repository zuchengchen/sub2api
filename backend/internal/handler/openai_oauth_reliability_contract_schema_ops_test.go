//go:build unit

package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOAuthReliabilityContractSchemaOpsTerminalOverridesPriorAttempt(t *testing.T) {
	for _, tc := range []struct {
		name, object string
		status       int
		typ          string
	}{
		{"schema_code", `{"code":"invalid_json_schema","param":"text.format.schema","message":"Synthetic strict schema rejection"}`, 400, "invalid_request_error"},
		{"schema_type", `{"type":"invalid_json_schema","param":"text.format.schema","message":"Synthetic strict schema rejection"}`, 400, "invalid_request_error"},
		{"unknown_schema_prose", `{"code":"future_provider_code","message":"Invalid schema for response_format 'synthetic': uniqueItems is not permitted when strict is true"}`, 502, "upstream_error"},
		{"duplicate_code", `{"code":"invalid_json_schema","code":"opaque_provider_failure","message":"Synthetic ambiguous error"}`, 502, "upstream_error"},
		{"duplicate_code_reverse", `{"code":"opaque_provider_failure","code":"invalid_json_schema","message":"Synthetic ambiguous error"}`, 502, "upstream_error"},
		{"duplicate_type", `{"type":"invalid_json_schema","type":"opaque_provider_failure","message":"Synthetic ambiguous error"}`, 502, "api_error"},
		{"duplicate_type_reverse", `{"type":"opaque_provider_failure","type":"invalid_json_schema","message":"Synthetic ambiguous error"}`, 502, "api_error"},
		{"real_capacity", `{"code":"server_is_overloaded","message":"synthetic capacity"}`, 503, "overloaded_error"},
		{"explicit_terminal_401", `{"code":"invalid_json_schema","status_code":401,"message":"contradictory explicit auth status is not silently rewritten"}`, 401, "upstream_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupOpsErrorLogTestQueue(t, 4)
			gin.SetMode(gin.TestMode)
			ops := service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil)
			router := gin.New()
			router.Use(OpsErrorLoggerMiddleware(ops))
			router.POST("/v1/responses", func(c *gin.Context) {
				// Same request can contain an earlier account failure. Final
				// semantics must come from the emitted event, never this state.
				service.SetOpsUpstreamError(c, http.StatusTooManyRequests, "prior synthetic attempt", "")
				service.MarkOpsStreamFailure(c, "rate_limit_error", "rate_limit_exceeded", "prior synthetic attempt", http.StatusTooManyRequests)
				c.Header("Content-Type", "text/event-stream")
				_, err := c.Writer.WriteString("data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":" + tc.object + "}}\n\n")
				require.NoError(t, err)
				c.Writer.Flush()
			})
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"synthetic"}`)))
			require.Equal(t, http.StatusOK, recorder.Code)
			require.Equal(t, int64(1), OpsErrorLogQueueLength())
			job := <-opsErrorLogQueue
			require.Equal(t, tc.status, job.entry.StatusCode, "captured terminal is authoritative, not prior429 or wire200")
			require.Equal(t, tc.typ, job.entry.ErrorType)
		})
	}
}

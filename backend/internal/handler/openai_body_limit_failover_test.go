package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Every account refused the body size: the only remedy is a shorter
// conversation, so the client is told context_length_exceeded (Codex compacts
// only on a streamed response.failed with that code).
func TestOpenAIBodyLimitFailoverExhausted_NonStreamResponsesReturnsContextLengthExceeded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))
	service.MarkOpenAIResponsesClientStream(c, false)

	(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, bodyLimitFailoverTestError(), false)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	errBody, ok := envelope["error"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "invalid_request_error", errBody["type"])
	require.Equal(t, service.OpenAIContextLengthExceededCode, errBody["code"])
	require.NotContains(t, rec.Body.String(), "must-not-leak")
}

func TestOpenAIBodyLimitFailoverExhausted_StreamResponsesReturnsFailedEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, streamStarted := range []bool{false, true} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(nil))
		service.MarkOpenAIResponsesClientStream(c, true)

		(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, bodyLimitFailoverTestError(), streamStarted)

		body := rec.Body.String()
		require.Equal(t, http.StatusOK, rec.Code)
		require.True(t, strings.HasPrefix(body, "event: response.failed\n"), body)
		require.Equal(t, 1, strings.Count(body, "event: response.failed"))
		require.Contains(t, body, `"code":"context_length_exceeded"`)
		require.NotContains(t, body, "must-not-leak")
	}
}

func TestOpenAIBodyLimitFailoverExhausted_ChatCompletionsReturnsContextLengthExceeded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(nil))

	(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, bodyLimitFailoverTestError(), false)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"context_length_exceeded"`)
	require.NotContains(t, rec.Body.String(), "must-not-leak")
}

func bodyLimitFailoverTestError() *service.UpstreamFailoverError {
	return &service.UpstreamFailoverError{
		StatusCode:        http.StatusRequestEntityTooLarge,
		ResponseBody:      []byte(`{"error":{"message":"proxy limit secret=must-not-leak"}}`),
		Scope:             service.GatewayFailureScopeAccount,
		Reason:            service.GatewayFailureReason("openai_request_body_too_large"),
		NextAccountAction: service.NextAccountRetry,
		ClientStatusCode:  http.StatusRequestEntityTooLarge,
		ClientMessage:     "Request payload is too large",
	}
}

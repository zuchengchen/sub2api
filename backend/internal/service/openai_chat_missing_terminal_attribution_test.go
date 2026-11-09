package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestChatBufferedMissingTerminalKeepsFailureAndAttributesUpstream(t *testing.T) {
	for _, tc := range []struct {
		name, stream string
		canceled     bool
	}{
		{name: "empty_eof"},
		{name: "done_only", stream: "data: [DONE]\n\n"},
		{name: "metadata_only", stream: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_missing\",\"status\":\"in_progress\"}}\n\n"},
		{name: "partial_text", stream: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial text\"}\n\n"},
		{name: "tool_call_started", stream: "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"send_notification\"}}\n\n"},
		{name: "usage_before_eof", stream: "data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_missing\",\"status\":\"in_progress\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n"},
		{name: "client_canceled", canceled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"upstream-terminal-missing"}}, Body: io.NopCloser(strings.NewReader(tc.stream))}
			account := &Account{ID: 40, Name: "synthetic-account", Platform: PlatformOpenAI}
			result, err := (&OpenAIGatewayService{}).handleChatBufferedStreamingResponse(resp, c, account, "gpt-5.6-sol", "gpt-5.6-sol", "gpt-5.6-sol", time.Now())
			require.Error(t, err)
			if tc.name == "usage_before_eof" {
				require.NotNil(t, result)
				require.Equal(t, 3, result.Usage.InputTokens)
				require.Equal(t, 2, result.Usage.OutputTokens)
			} else {
				require.Nil(t, result)
			}
			var failover *UpstreamFailoverError
			require.NotErrorAs(t, err, &failover, "attribution repair must not add replay after partial upstream work or cancellation")
			require.Equal(t, http.StatusBadGateway, recorder.Code)
			require.Equal(t, "api_error", gjson.GetBytes(recorder.Body.Bytes(), "error.type").String(), "client contract is preserved")
			require.Equal(t, "Upstream stream ended without a terminal response event", gjson.GetBytes(recorder.Body.Bytes(), "error.message").String())
			require.Equal(t, http.StatusBadGateway, c.GetInt(OpsUpstreamStatusCodeKey), "confirmed upstream EOF must reach Ops attribution")
			events, ok := c.Get(OpsUpstreamErrorsKey)
			require.True(t, ok)
			attempts, ok := events.([]*OpsUpstreamErrorEvent)
			require.True(t, ok)
			require.Len(t, attempts, 1)
			require.Equal(t, account.ID, attempts[0].AccountID)
			require.Equal(t, "upstream-terminal-missing", attempts[0].UpstreamRequestID)
			require.Equal(t, http.StatusBadGateway, attempts[0].UpstreamStatusCode)
		})
	}
}

func TestChatBufferedCompletedDoesNotRecordUpstreamFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	stream := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_completed\",\"status\":\"completed\",\"model\":\"gpt-5.6-sol\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n"
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}
	result, err := (&OpenAIGatewayService{}).handleChatBufferedStreamingResponse(resp, c, &Account{ID: 40, Platform: PlatformOpenAI}, "gpt-5.6-sol", "gpt-5.6-sol", "gpt-5.6-sol", time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "ok", gjson.GetBytes(recorder.Body.Bytes(), "choices.0.message.content").String())
	_, exists := c.Get(OpsUpstreamErrorsKey)
	require.False(t, exists)
	_, exists = c.Get(OpsUpstreamStatusCodeKey)
	require.False(t, exists)
}

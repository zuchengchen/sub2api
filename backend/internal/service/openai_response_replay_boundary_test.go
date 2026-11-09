package service

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Synthetic audit regression: no actual upstream requests, credentials, or DB.
// These assertions express the desired replay boundary, not the current behavior.
func TestGatewayAudit_MeteredTerminalMustNotReplay(t *testing.T) {
	payload := `{"type":"response.failed","response":{"id":"resp_synthetic","status":"failed","output":[],"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."},"usage":{"input_tokens":8,"output_tokens":2}}}`
	for _, route := range []string{"responses_stream", "responses_passthrough_stream", "responses_buffered", "responses_passthrough_buffered", "chat_stream", "chat_buffered", "messages_stream", "messages_buffered"} {
		t.Run(route, func(t *testing.T) {
			usage, err := gatewayAuditRunRoute(t, route, "data: "+payload+"\n\n")
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover), "metered upstream work must not be replayed; got %v", err)
			require.NotNil(t, usage, "known upstream usage must survive terminal failure")
			require.Equal(t, 8, usage.InputTokens)
			require.Equal(t, 2, usage.OutputTokens)
		})
	}
}

func TestGatewayAudit_BufferedProgressMustNotReplay(t *testing.T) {
	failed := `{"type":"response.failed","response":{"id":"resp_synthetic","status":"failed","output":[],"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}}`
	for _, route := range []string{"responses_buffered", "responses_passthrough_buffered", "chat_buffered", "messages_buffered"} {
		for _, tc := range []struct{ name, prior string }{
			{"text", `{"type":"response.output_text.delta","item_id":"msg_synthetic","output_index":0,"content_index":0,"delta":"synthetic output"}`},
			{"hosted_tool", `{"type":"response.output_item.added","output_index":0,"item":{"id":"ws_synthetic","type":"web_search_call","status":"in_progress"}}`},
			{"prior_usage", `{"type":"response.in_progress","response":{"id":"resp_synthetic","usage":{"input_tokens":8,"output_tokens":2}}}`},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				_, err := gatewayAuditRunRoute(t, route, "data: "+tc.prior+"\n\ndata: "+failed+"\n\n")
				require.Error(t, err)
				var failover *UpstreamFailoverError
				require.False(t, errors.As(err, &failover), "buffered upstream progress is a replay boundary even if the client saw no bytes; got %v", err)
			})
		}
	}
}

func TestGatewayAudit_UnstartedCapacityKeepsExistingRecovery(t *testing.T) {
	payload := `{"type":"response.failed","response":{"id":"resp_synthetic","status":"failed","output":[],"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}}`
	for _, route := range []string{"responses_stream", "responses_passthrough_stream", "responses_buffered", "responses_passthrough_buffered", "chat_stream", "chat_buffered", "messages_stream", "messages_buffered"} {
		t.Run(route, func(t *testing.T) {
			_, err := gatewayAuditRunRoute(t, route, "data: "+payload+"\n\n")
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.True(t, failover.RetryableOnSameAccount)
			require.True(t, failover.RequestScopedTransient)
		})
	}
}

func gatewayAuditRunRoute(t *testing.T, route, body string) (*OpenAIUsage, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Name: "synthetic-audit"}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"synthetic-audit"}}, Body: io.NopCloser(strings.NewReader(body))}
	defer resp.Body.Close()
	start := time.Now()
	switch route {
	case "responses_stream":
		result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, start, "model", "model")
		if result == nil {
			return nil, err
		}
		return result.usage, err
	case "responses_passthrough_stream":
		result, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, start, "model", "model")
		if result == nil {
			return nil, err
		}
		return result.usage, err
	case "responses_buffered":
		result, err := svc.handleSSEToJSON(resp, c, account, []byte(body), "model", "model")
		if result == nil {
			return nil, err
		}
		return result.usage, err
	case "responses_passthrough_buffered":
		result, err := svc.handlePassthroughSSEToJSON(resp, c, account, []byte(body), "model", "model")
		if result == nil {
			return nil, err
		}
		return result.usage, err
	case "chat_stream":
		result, err := svc.handleChatStreamingResponse(resp, c, account, "model", "model", "model", start, nil)
		if result == nil {
			return nil, err
		}
		return &result.Usage, err
	case "chat_buffered":
		result, err := svc.handleChatBufferedStreamingResponse(resp, c, account, "model", "model", "model", start)
		if result == nil {
			return nil, err
		}
		return &result.Usage, err
	case "messages_stream":
		result, err := svc.handleAnthropicStreamingResponse(resp, c, account, "model", "model", "model", start)
		if result == nil {
			return nil, err
		}
		return &result.Usage, err
	case "messages_buffered":
		result, err := svc.handleAnthropicBufferedStreamingResponse(resp, c, account, "model", "model", "model", start)
		if result == nil {
			return nil, err
		}
		return &result.Usage, err
	default:
		t.Fatalf("unknown synthetic route %s", route)
		return nil, nil
	}
}

func TestGatewayAudit_ForwardRetainsMeteredFailure(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			name := "native_buffered"
			if passthrough {
				name = "passthrough_buffered"
			}
			if stream {
				name = strings.TrimSuffix(name, "buffered") + "stream"
			}
			t.Run(name, func(t *testing.T) {
				body := "data: " + `{"type":"response.failed","response":{"id":"resp_synthetic","status":"failed","output":[],"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."},"usage":{"input_tokens":8,"output_tokens":2}}}` + "\n\n"
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}}
				svc := newOpenAIImageGenerationControlTestService(upstream)
				c, _ := newOpenAIImageGenerationControlTestContext(true, "synthetic-audit")
				account := newOpenAIImageGenerationControlTestAccount()
				account.Extra = map[string]any{"openai_passthrough": passthrough}
				requestBody := []byte(`{"model":"gpt-5.6-sol","stream":false,"input":"synthetic"}`)
				if stream {
					requestBody = []byte(`{"model":"gpt-5.6-sol","stream":true,"input":"synthetic"}`)
				}
				result, err := svc.Forward(c.Request.Context(), c, account, requestBody)
				require.Error(t, err)
				var failover *UpstreamFailoverError
				require.False(t, errors.As(err, &failover), "full Forward must not replay a metered failure")
				require.NotNil(t, result, "full Forward must preserve actual usage on failure")
				require.Equal(t, 8, result.Usage.InputTokens)
				require.Equal(t, 2, result.Usage.OutputTokens)
			})
		}
	}
}

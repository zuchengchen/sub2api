package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGPT61SolChatCacheDeliveryDiagnosticHTTP(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(strconvBoolForUserCacheTest(stream), func(t *testing.T) {
			path := userCacheDiagnosticTestEnv(t)
			terminal := `{"type":"response.completed","response":{"id":"PRIVATE_RESPONSE","model":"gpt-6.1-sol","status":"completed","output":[{"id":"PRIVATE_MESSAGE","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"PRIVATE_ANSWER"}]}],"usage":{"input_tokens":10000,"input_tokens_details":{"cached_tokens":9500},"output_tokens":30,"total_tokens":10030}}}`
			wire := "data: " + `{"type":"response.created","response":{"id":"PRIVATE_RESPONSE","model":"gpt-6.1-sol","status":"in_progress"}}` + "\n\n" +
				"data: " + `{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"PRIVATE_ANSWER"}` + "\n\n" +
				"data: " + terminal + "\n\ndata: [DONE]\n\n"
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}}
			svc := &OpenAIGatewayService{cfg: codexSynthEnabledConfig(), httpUpstream: upstream, tiboRouteDisabled: true}
			account := newCodexSynthTestAccount(23157, AccountTypeOAuth)
			completed := make(chan error, 1)
			router := gin.New()
			router.POST("/v1/chat/completions", func(c *gin.Context) {
				c.Set("api_key", &APIKey{ID: 794, UserID: 428})
				body, err := io.ReadAll(c.Request.Body)
				if err == nil {
					_, err = svc.ForwardAsChatCompletions(c.Request.Context(), c, account, body, "", "")
				}
				completed <- err
			})
			server := httptest.NewServer(router)
			defer server.Close()
			body := `{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"PRIVATE_PROMPT"}],"stream":` + strconvBoolForUserCacheTest(stream) + `}`
			response, err := server.Client().Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
			require.NoError(t, err)
			raw, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.NoError(t, <-completed)
			require.Equal(t, 200, response.StatusCode)
			require.Contains(t, string(raw), "PRIVATE_ANSWER")
			if stream {
				require.True(t, strings.HasSuffix(string(raw), "data: [DONE]\n\n"))
				require.Contains(t, string(raw), `"finish_reason":"stop"`)
			} else {
				require.Equal(t, "PRIVATE_RESPONSE", gjson.GetBytes(raw, "id").String())
				require.Equal(t, "stop", gjson.GetBytes(raw, "choices.0.finish_reason").String())
			}
			events := userCacheDiagnosticReadEvents(t, path)
			require.Len(t, events, 4)
			var delivery, upstreamEvent map[string]any
			for _, event := range events {
				switch event["stage"] {
				case "chat_delivery":
					delivery = event
				case "upstream_response":
					upstreamEvent = event
				}
			}
			require.NotNil(t, delivery)
			require.NotNil(t, upstreamEvent)
			require.Equal(t, true, delivery["forward_returned_success"])
			require.Equal(t, true, delivery["downstream_delivered_complete"])
			require.Equal(t, upstreamEvent["response_id_sha256"], delivery["consumer_response_id_sha256"])
			require.Equal(t, upstreamEvent["trace_id"], delivery["trace_id"])
			require.Equal(t, "off", delivery["native_mode"], "bounded diagnostics work before enabling replay")
			usage := delivery["consumer_usage"].(map[string]any)
			require.EqualValues(t, 10000, usage["input_tokens"])
			require.EqualValues(t, 9500, usage["cache_read_input_tokens"])
			require.EqualValues(t, 30, usage["output_tokens"])
			log, err := os.ReadFile(path)
			require.NoError(t, err)
			for _, secret := range []string{"PRIVATE_RESPONSE", "PRIVATE_MESSAGE", "PRIVATE_ANSWER", "PRIVATE_PROMPT", "oauth-token"} {
				require.NotContains(t, string(log), secret)
			}
		})
	}
}

func TestGPT61SolChatCacheDeliveryDiagnosticKeepsFailures(t *testing.T) {
	for _, scenario := range []string{"incomplete", "short_write", "forward_error", "canceled", "missing_capture", "missing_result"} {
		t.Run(scenario, func(t *testing.T) {
			path := userCacheDiagnosticTestEnv(t)
			c := userCacheDiagnosticTestContext(428)
			diagnostic := beginUserCacheDiagnostic(c, []byte(`{"model":"gpt-6.1-sol"}`))
			writer := c.Writer
			if scenario == "short_write" {
				writer = &nativeCaptureFaultWriter{ResponseWriter: writer, short: true}
			}
			delivery := NewNativeReasoningChatCapture(writer, true, DefaultNativeReasoningReplayLimits())
			wire := nativeCaptureChatWire()
			if scenario == "incomplete" {
				wire = []byte(strings.TrimSuffix(string(wire), "data: [DONE]\n\n"))
			}
			_, _ = delivery.Write(wire)
			var forwardErr error
			if scenario == "forward_error" {
				forwardErr = errors.New("PRIVATE_FAILURE")
			}
			if scenario == "canceled" {
				ctx, cancel := context.WithCancel(c.Request.Context())
				cancel()
				c.Request = c.Request.WithContext(ctx)
			}
			if scenario == "missing_capture" {
				delivery = nil
			}
			result := &OpenAIForwardResult{Stream: true, Usage: OpenAIUsage{InputTokens: 100, OutputTokens: 2}}
			if scenario == "missing_result" {
				result = nil
			}
			(&userCacheDiagnosticAttempt{diagnostic: diagnostic, accountID: 23157, attempt: 1, usageRequestIDHash: diagnostic.usageRequestIDHash}).chatDelivery(c, delivery, result, forwardErr, nil)
			events := userCacheDiagnosticReadEvents(t, path)
			require.Len(t, events, 2)
			event := events[1]
			switch scenario {
			case "incomplete", "short_write", "missing_capture":
				require.Equal(t, false, event["downstream_delivered_complete"])
				require.NotContains(t, event, "consumer_response_id_sha256")
			case "forward_error", "missing_result":
				require.Equal(t, false, event["forward_returned_success"])
			case "canceled":
				require.Equal(t, true, event["context_canceled"])
			}
			log, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NotContains(t, string(log), "PRIVATE_FAILURE")
		})
	}
}

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func imageUserErrorTestEvent(t *testing.T, eventType, errorType string, metered bool) []byte {
	t.Helper()
	errorBody := map[string]any{"type": errorType, "code": "invalid_value", "param": "output_format", "message": "GIF output is not enabled for this request."}
	event := map[string]any{"type": eventType, "sequence_number": 2, "error": errorBody}
	if eventType == "response.failed" {
		delete(event, "error")
		response := map[string]any{"id": "resp_image_denied", "status": "failed", "error": errorBody, "output": []any{}}
		if metered {
			response["usage"] = map[string]int{"input_tokens": 8, "output_tokens": 2}
		}
		event["response"] = response
	}
	result, err := json.Marshal(event)
	require.NoError(t, err)
	return result
}

func requireImageUserErrorResponse(t *testing.T, body []byte, errorType string) {
	t.Helper()
	value := gjson.GetBytes(body, "response.error")
	if !value.Exists() {
		value = gjson.GetBytes(body, "error")
	}
	require.Equal(t, errorType, value.Get("type").String())
	require.Equal(t, "invalid_value", value.Get("code").String())
	require.Equal(t, "output_format", value.Get("param").String())
	require.Equal(t, "GIF output is not enabled for this request.", value.Get("message").String())
}

func TestOpenAIImageUserErrorWSResponses(t *testing.T) {
	for _, errorType := range []string{"image_generation_user_error", "image_generation_user_err"} {
		for _, eventType := range []string{"error", "response.failed"} {
			for _, mode := range []string{"json", "sse", "sse_after_output", "json_with_usage"} {
				t.Run(errorType+"/"+eventType+"/"+mode, func(t *testing.T) {
					stream := strings.HasPrefix(mode, "sse")
					metered := mode == "json_with_usage" && eventType == "response.failed"
					gin.SetMode(gin.TestMode)
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					c.Request.Header.Set("User-Agent", "unit-test-agent/1.0")
					cfg := &config.Config{}
					cfg.Gateway.OpenAIWS.Enabled = true
					cfg.Gateway.OpenAIWS.OAuthEnabled = true
					cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
					cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
					cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
					events := [][]byte{[]byte(`{"type":"response.created","sequence_number":0,"response":{"id":"resp_image_denied","model":"gpt-5.1","status":"in_progress"}}`)}
					if mode == "sse_after_output" {
						events = append(events, []byte(`{"type":"response.output_text.delta","sequence_number":1,"delta":"Preparing image"}`))
					}
					events = append(events, imageUserErrorTestEvent(t, eventType, errorType, metered))
					conn := &openAIWSCaptureConn{events: events}
					pool := newOpenAIWSConnPool(cfg)
					pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: conn})
					defer pool.Close()
					upstream := &httpUpstreamRecorder{}
					svc := &OpenAIGatewayService{tiboRouteDisabled: true, cfg: cfg, httpUpstream: upstream, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), openaiWSPool: pool}
					account := &Account{ID: 85, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"access_token": "test-token"}, Extra: astraHealthyTestExtra(map[string]any{"responses_websockets_v2_enabled": true})}
					request := []byte(fmt.Sprintf(`{"model":"gpt-5.1","stream":%t,"input":"draw an animation","tools":[{"type":"image_generation","model":"gpt-image-2.6-flare","output_format":"gif"}]}`, stream))
					result, err := svc.Forward(context.Background(), c, account, request)
					require.Error(t, err)
					var failover *UpstreamFailoverError
					require.NotErrorAs(t, err, &failover)
					require.Empty(t, upstream.requests, "image errors must not trigger an HTTP retry")
					require.Len(t, conn.writes, 1, "image errors must not reconnect and resend")
					require.True(t, IsResponseCommitted(c), "handler must not append a generic error")
					require.Equal(t, http.StatusBadRequest, c.GetInt(OpsUpstreamStatusCodeKey))
					if metered {
						require.NotNil(t, result)
						require.Equal(t, 8, result.Usage.InputTokens)
						require.Equal(t, 2, result.Usage.OutputTokens)
					}
					if !stream {
						require.Equal(t, http.StatusBadRequest, rec.Code)
						requireImageUserErrorResponse(t, rec.Body.Bytes(), errorType)
						return
					}
					terminalCount := 0
					forEachOpenAISSEFrame(rec.Body.String(), func(kind string, payload []byte) {
						if kind != "response.failed" {
							return
						}
						terminalCount++
						requireImageUserErrorResponse(t, payload, errorType)
						require.Equal(t, "resp_image_denied", gjson.GetBytes(payload, "response.id").String())
					})
					require.Equal(t, 1, terminalCount)
					ops, ok := GetOpsStreamError(c)
					require.True(t, ok)
					require.Equal(t, http.StatusBadRequest, ops.IntendedStatus)
					require.Equal(t, errorType, ops.ErrType)
					require.Equal(t, "invalid_value", ops.Code)
				})
			}
		}
	}
}

func TestOpenAIImageUserErrorHTTPResponses(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, eventType := range []string{"error", "response.failed"} {
			for _, errorType := range []string{"image_generation_user_error", "image_generation_user_err"} {
				t.Run(fmt.Sprintf("passthrough=%t/%s/%s", passthrough, eventType, errorType), func(t *testing.T) {
					c, rec := newNonStreamingFailoverContext(t)
					svc := newNonStreamingFailoverService()
					account := newNonStreamingFailoverAccount()
					body := sseTerminalBody(eventType, string(imageUserErrorTestEvent(t, eventType, errorType, false)))
					var err error
					if passthrough {
						_, err = svc.handlePassthroughSSEToJSON(newNonStreamingSSEResponse(), c, account, body, "gpt-5.1", "gpt-5.1")
					} else {
						_, err = svc.handleSSEToJSON(newNonStreamingSSEResponse(), c, account, body, "gpt-5.1", "gpt-5.1")
					}
					require.Error(t, err)
					var failover *UpstreamFailoverError
					require.NotErrorAs(t, err, &failover)
					require.Equal(t, http.StatusBadRequest, rec.Code)
					requireImageUserErrorResponse(t, rec.Body.Bytes(), errorType)
					require.Equal(t, http.StatusBadRequest, c.GetInt(OpsUpstreamStatusCodeKey))
					require.True(t, IsResponseCommitted(c))
				})
			}
		}
	}
}

func TestOpenAIImageUserErrorSSEDoesNotFailover(t *testing.T) {
	for _, mode := range []string{"apikey", "oauth", "passthrough"} {
		for _, eventType := range []string{"error", "response.failed"} {
			t.Run(mode+"/"+eventType, func(t *testing.T) {
				c, rec := newNonStreamingFailoverContext(t)
				svc := newNonStreamingFailoverService()
				svc.toolCorrector = NewCodexToolCorrector()
				account := newNonStreamingFailoverAccount()
				if mode == "oauth" {
					account.Type = AccountTypeOAuth
				}
				body := sseTerminalBody(eventType, string(imageUserErrorTestEvent(t, eventType, "image_generation_user_error", false)))
				resp := newNonStreamingSSEResponse()
				resp.Body = io.NopCloser(bytes.NewReader(body))
				var err error
				if mode == "passthrough" {
					_, err = svc.handleStreamingResponsePassthrough(context.Background(), resp, c, account, time.Now(), "gpt-5.1", "gpt-5.1")
				} else {
					_, err = svc.handleStreamingResponse(context.Background(), resp, c, account, time.Now(), "gpt-5.1", "gpt-5.1")
				}
				require.Error(t, err)
				var failover *UpstreamFailoverError
				require.NotErrorAs(t, err, &failover)
				terminalCount := 0
				forEachOpenAISSEFrame(rec.Body.String(), func(kind string, payload []byte) {
					if kind == "response.failed" {
						terminalCount++
						requireImageUserErrorResponse(t, payload, "image_generation_user_error")
					}
				})
				require.Equal(t, 1, terminalCount)
				require.Equal(t, http.StatusBadRequest, c.GetInt(OpsUpstreamStatusCodeKey))
				ops, ok := GetOpsStreamError(c)
				require.True(t, ok)
				require.Equal(t, http.StatusBadRequest, ops.IntendedStatus)
				require.Equal(t, "invalid_value", ops.Code)
			})
		}
	}
}

func TestOpenAIImageUserErrorDoesNotRetryBasedOnMessage(t *testing.T) {
	payload := []byte(`{"type":"response.failed","response":{"error":{"type":"image_generation_user_error","code":"invalid_value","message":"Temporary GIF access is unavailable; please retry after enabling preview."}}}`)
	require.False(t, openAIStreamFailedEventShouldFailover(payload, extractOpenAISSEErrorMessage(payload)))
	require.False(t, openAIStreamErrorEventShouldFailover(payload, extractOpenAISSEErrorMessage(payload)))
	require.Equal(t, http.StatusBadRequest, openAIStreamFailureStatus(payload, extractOpenAISSEErrorMessage(payload)))
	for _, kind := range []string{"image_generation_user_error", "image_generation_user_err"} {
		require.Equal(t, http.StatusBadRequest, openAIWSErrorHTTPStatusFromRaw("invalid_value", kind))
		require.Equal(t, http.StatusBadRequest, openAIImagesSSEErrorStatus(kind, "invalid_value"))
	}
}

func TestOpenAIImageUserErrorAfterKeepalive(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprint(compact), func(t *testing.T) {
			c, rec := newCompactBridgeTestContext(t, compact)
			stop := startOpenAISSEKeepalive(c, time.Hour)
			defer stop()
			value, ok := c.Get(openAICompactSSEKeepaliveKey)
			require.True(t, ok)
			require.True(t, value.(*openAICompactSSEKeepalive).beat())
			svc := newNonStreamingFailoverService()
			payload := imageUserErrorTestEvent(t, "error", "image_generation_user_error", false)
			_, err := svc.handleSSEToJSON(newNonStreamingSSEResponse(), c, newNonStreamingFailoverAccount(), sseTerminalBody("error", string(payload)), "gpt-5.1", "gpt-5.1")
			require.Error(t, err)
			require.Equal(t, 200, rec.Code, "already committed status must not be rewritten")
			require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
			count := 0
			forEachOpenAISSEFrame(rec.Body.String(), func(kind string, body []byte) {
				if kind == "response.failed" {
					count++
					requireImageUserErrorResponse(t, body, "image_generation_user_error")
				}
			})
			require.Equal(t, 1, count)
			require.True(t, IsResponseCommitted(c))
			ops, ok := GetOpsStreamError(c)
			require.True(t, ok)
			require.Equal(t, 400, ops.IntendedStatus)
			require.Equal(t, "invalid_value", ops.Code)
		})
	}
}

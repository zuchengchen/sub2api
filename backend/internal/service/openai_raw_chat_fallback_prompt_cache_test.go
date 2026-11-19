//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGPTChatFallbackPromptCacheForwarding(t *testing.T) {
	const model = "gpt-4.1-mini"
	for _, path := range []string{"/v1/responses", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			account := forceChatResponsesFallbackAccount()
			account.Credentials["base_url"] = "https://api.openai.com"
			upstream := &httpUpstreamRecorder{}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			forward := func(keyID int64, first string, followup bool, key, session, metadata string) string {
				t.Helper()
				history := []map[string]string{{"role": "user", "content": first}}
				if followup {
					history = append(history, map[string]string{"role": "assistant", "content": "OK"}, map[string]string{"role": "user", "content": "Continue"})
				}
				payload := map[string]any{"model": model}
				if path == "/v1/responses" {
					payload["input"] = history
					if !followup {
						payload["input"] = first
					}
				} else {
					payload["messages"] = history
					payload["max_tokens"] = 32
					if metadata != "" {
						payload["metadata"] = map[string]string{"user_id": metadata}
					}
				}
				if key != "" {
					payload["prompt_cache_key"] = key
				}
				body, err := json.Marshal(payload)
				require.NoError(t, err)
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
				c.Set("api_key", &APIKey{ID: keyID})
				if session != "" {
					c.Request.Header.Set("session-id", session)
				}
				upstream.resp = &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"chatcmpl_local","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":1,"total_tokens":9,"prompt_tokens_details":{"cached_tokens":4}}}`))}
				var result *OpenAIForwardResult
				if path == "/v1/responses" {
					result, err = svc.Forward(context.Background(), c, account, body)
				} else {
					result, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, svc.ExtractSessionID(c, body), "")
				}
				require.NoError(t, err)
				require.Equal(t, "/v1/chat/completions", upstream.lastReq.URL.Path)
				require.Equal(t, first, gjson.GetBytes(upstream.lastBody, "messages.0.content").String())
				require.Equal(t, 4, result.Usage.CacheReadInputTokens)
				if upstream.lastReq.URL.Hostname() == "api.openai.com" {
					require.Empty(t, upstream.lastReq.Header.Get("session-id"))
				} else {
					require.Equal(t, session, upstream.lastReq.Header.Get("session-id"))
				}
				require.False(t, gjson.GetBytes(upstream.lastBody, "previous_response_id").Exists())
				return gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String()
			}

			key := forward(101, "First question", false, "", "", "")
			require.NotEmpty(t, key)
			require.Equal(t, key, forward(101, "First question", true, "", "", ""))
			require.NotEqual(t, key, forward(102, "First question", true, "", "", ""))
			require.NotEqual(t, key, forward(101, "Another question", false, "", "", ""))
			require.Equal(t, "caller-prefix", forward(101, "First question", true, "caller-prefix", "separate-session", ""))
			require.Equal(t, isolateOpenAISessionID(101, "client-thread"), forward(101, "First question", true, "", "client-thread", ""))
			if path == "/v1/messages" {
				const metadata = `{"device_id":"local-device","session_id":"local-thread"}`
				metaKey := forward(101, "First question", false, "", "", metadata)
				require.NotEqual(t, key, metaKey)
				require.Equal(t, metaKey, forward(101, "Another question", true, "", "", metadata))
			}
			account.Credentials["base_url"] = "https://api.openai.com.third-party.example"
			require.Empty(t, forward(101, "First question", false, "", "", ""))
			require.Empty(t, forward(101, "First question", false, "caller-prefix", "separate-session", ""))
		})
	}
}

//go:build unit

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

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGPTChatModelAndCacheRouting(t *testing.T) {
	for _, model := range []string{"gpt-3.5-turbo", "gpt-4", "gpt-4o-mini", "gpt-4.1-2025-04-14", "gpt-5.5", "gpt-6.1-terra", "gpt-7.2-mini", "gpt-daybreak-blue-latest"} {
		require.True(t, isCanonicalGPTChatModel(model), model)
	}
	for _, model := range []string{"", "GPT-5.5", " gpt-5.5", "openai/gpt-4o", "claude-gpt-5", "gpt-", "gpt-5/other", "gpt-5..6", "gpt-6.2-*", "gpt-image-2", "gpt-4o-audio-preview", "gpt-4o-mini-transcribe", "gpt-realtime", "gpt-oss-120b", "gpt-daybreak-unknown", "gpt-daybreak-blue-latest-high", "o3", "grok-4.5"} {
		require.False(t, isCanonicalGPTChatModel(model), model)
	}
	for _, model := range []string{"gpt-4o", "gpt-4.1-mini", "gpt-5.2", "gpt-6.1-terra", "gpt-7.2-mini", "gpt-daybreak-red-latest"} {
		require.True(t, supportsOpenAIChatPromptCacheKey(model), model)
	}
	for _, model := range []string{"gpt-3.5-turbo", "gpt-4-turbo", "gpt-image-2", "gpt-4o-realtime-preview", "claude-sonnet-4", ""} {
		require.False(t, supportsOpenAIChatPromptCacheKey(model), model)
	}
}

func TestGPTChatPromptCacheForwarding(t *testing.T) {
	for _, tc := range []struct {
		name, model                   string
		raw, official, oauth, autoKey bool
	}{
		{"official_responses", "gpt-4o-mini", false, true, false, true},
		{"codex_responses", "gpt-6.1-terra", false, true, true, true},
		{"official_chat", "gpt-4.1-mini", true, true, false, true},
		{"third_party_chat", "gpt-4.1-mini", true, false, false, false},
		{"third_party_responses", "gpt-4.1-mini", false, false, false, false},
		{"classic_chat", "gpt-3.5-turbo", true, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := rawChatCompletionsTestAccount()
			account.Extra = map[string]any{"openai_responses_supported": !tc.raw}
			if tc.official {
				account.Credentials["base_url"] = "https://api.openai.com"
			}
			if tc.oauth {
				account = newOpenAIOAuthNamespaceTestAccount()
			}
			upstream := &httpUpstreamRecorder{}
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
			forward := func(keyID int64, followup bool, first, custom string) (string, string) {
				t.Helper()
				history := []map[string]string{{"role": "system", "content": "Stable instructions"}, {"role": "user", "content": first}}
				if followup {
					history = append(history, map[string]string{"role": "assistant", "content": "ok"}, map[string]string{"role": "user", "content": "Next question"})
				}
				payload := map[string]any{"model": tc.model, "messages": history}
				if custom != "" {
					payload["prompt_cache_key"] = custom
				}
				body, err := json.Marshal(payload)
				require.NoError(t, err)
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
				c.Set("api_key", &APIKey{ID: keyID, UserID: keyID})
				if custom != "" {
					c.Request.Header.Set("session-id", "separate-conversation")
				}
				hash := svc.GenerateChatCompletionsSessionHash(c, body, tc.model)
				if tc.raw {
					resp := fmt.Sprintf(`{"id":"chatcmpl_test","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":2,"total_tokens":52,"prompt_tokens_details":{"cached_tokens":20}}}`, tc.model)
					upstream.resp = &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(resp))}
				} else {
					upstream.resp = openAICompatSSECompletedResponse("resp_test", tc.model)
				}
				result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, svc.ExtractSessionID(c, body), "")
				require.NoError(t, err)
				require.Equal(t, tc.model, gjson.GetBytes(upstream.lastBody, "model").String())
				if tc.raw {
					require.EqualValues(t, 20, result.Usage.CacheReadInputTokens)
					require.Empty(t, upstream.lastReq.Header.Get("session_id"))
				}
				return gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String(), hash
			}
			firstKey, firstRoute := forward(101, false, "Inspect the first case", "")
			nextKey, nextRoute := forward(101, true, "Inspect the first case", "")
			require.NotEmpty(t, firstRoute)
			require.Equal(t, firstRoute, nextRoute)
			require.Equal(t, firstKey, nextKey)
			if !tc.autoKey {
				require.Empty(t, firstKey)
				return
			}
			require.NotEmpty(t, firstKey)
			if !tc.oauth {
				unscoped, _ := forward(0, false, "Inspect the first case", "")
				require.Empty(t, unscoped)
			}
			otherKey, _ := forward(102, true, "Inspect the first case", "")
			require.NotEqual(t, firstKey, otherKey)
			otherPrompt, otherRoute := forward(101, false, "Inspect another case", "")
			require.NotEqual(t, firstKey, otherPrompt)
			require.NotEqual(t, firstRoute, otherRoute)
			custom, _ := forward(101, true, "Inspect the first case", "customer-prefix")
			custom2, _ := forward(101, false, "Inspect another case", "customer-prefix")
			require.Equal(t, custom, custom2)
			if tc.oauth {
				require.Equal(t, scopeCodexAccountIdentityValue(account, 101, "prompt-cache", "customer-prefix"), custom)
			} else {
				require.Equal(t, "customer-prefix", custom)
			}
		})
	}
}
